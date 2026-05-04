// encrypt_sink.go: audit.Sink 的 envelope-encryption 包装。
//
// 用途（PCI-DSS / GDPR / 中国《个保法》合规）：
//   - 决策审计里有 IP / device_id / metadata 这些可能落到敏感数据范围
//   - 落 ClickHouse / Kafka / S3 时静态加密不够（数据是明文 row）；envelope
//     加密让"持有数据库副本 ≠ 能读"
//
// 设计：
//   - 主体：把 DecisionAudit 序列化成 JSON，AES-256-GCM 加密 → 替换原 record，
//     存成 EncryptedAudit{decision_id, occurred_at, key_id, nonce_b64, ciphertext_b64}
//   - decision_id / occurred_at 留明文，方便 by-id 查询 + retention partition
//   - 密钥来源：KeyProvider 接口；默认 EnvKeyProvider 从 RISK_AUDIT_KEY_<id>
//     环境变量读 32-byte hex；生产走 KMS DEK 缓存
//   - Key rotation：写新 key 用最新 active；读旧 record 按 record.KeyID
//     找历史 key（DataKey(keyID) → []byte）
//
// 密钥生成（一次性）：
//
//	openssl rand -hex 32
//
// 写入流向：
//
//	service.Screen → audit.MultiSink {
//	    LogSink (Header only, no PII),
//	    EncryptSink wrap CHSink (full record, encrypted),
//	    EncryptSink wrap S3 long-term archive,
//	}

package audit

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
)

// KeyProvider 拿 keyID 对应的 32-byte AES-256 key。生产实现连 KMS（aws kms
// Decrypt + 内存 cache，CMK 加密 DEK）。
type KeyProvider interface {
	// ActiveKeyID 当前应该用于加密的 key id（写路径用）
	ActiveKeyID() string
	// DataKey 拿 keyID 对应的 32-byte 解密 key（读 / 写都调）
	DataKey(keyID string) ([]byte, error)
}

// EnvKeyProvider 从环境变量读 key。命名规则：
//
//	ACTIVE_KEY_ID            = "v3"
//	RISK_AUDIT_KEY_v3        = "<64 hex chars>"
//	RISK_AUDIT_KEY_v2        = "<64 hex chars>"   # 旧版用于解密
//
// 仅适合开发 / 单机部署；生产走 KMSKeyProvider（本包不实现，调用方插）。
type EnvKeyProvider struct {
	ActiveID string
}

func NewEnvKeyProvider() *EnvKeyProvider {
	return &EnvKeyProvider{ActiveID: os.Getenv("ACTIVE_KEY_ID")}
}

func (p *EnvKeyProvider) ActiveKeyID() string {
	if p.ActiveID != "" {
		return p.ActiveID
	}
	return "v1"
}

func (p *EnvKeyProvider) DataKey(keyID string) ([]byte, error) {
	if keyID == "" {
		return nil, errors.New("audit encrypt: empty key id")
	}
	hexKey := os.Getenv("RISK_AUDIT_KEY_" + keyID)
	if hexKey == "" {
		return nil, fmt.Errorf("audit encrypt: key %q not configured", keyID)
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("audit encrypt: key %q decode: %w", keyID, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("audit encrypt: key %q wrong length %d (want 32)", keyID, len(key))
	}
	return key, nil
}

// EncryptedAudit 加密落库的 record schema。decision_id / occurred_at 明文便于
// by-id 查找 + 时间分区。其余敏感字段（input + hits + ml*）全打包进 ciphertext。
type EncryptedAudit struct {
	DecisionID  string    `json:"decision_id"`
	OccurredAt  time.Time `json:"occurred_at"`
	Verdict     string    `json:"verdict"`           // 明文：metrics 聚合用
	RiskScore   int       `json:"risk_score"`        // 明文：metrics 聚合用
	KeyID       string    `json:"key_id"`            // 哪个 DEK 加的密
	Nonce       string    `json:"nonce_b64"`         // GCM nonce（每条独立）
	Ciphertext  string    `json:"ciphertext_b64"`    // AES-256-GCM(json.Marshal(*DecisionAudit))
}

// EncryptSink 包装一个 inner Sink，把 DecisionAudit 加密后再交付。
//
//	inner.Write(ctx, &enc) — 收到的是 *DecisionAudit；EncryptSink 在内部
//	        把它 marshal + encrypt + 装回 DecisionAudit.{DecisionID, Verdict,
//	        RiskScore, OccurredAt, Hits=[加密元数据]}。
//
// 这里采用"侵入最小"策略：把加密载荷放进 hits[0].Detail（原 hits 仍保留以
// 兼容查询面板的"哪些规则命中"），key_id / nonce 落 metadata。生产可直接
// 让 inner sink 接 EncryptedAudit struct（需要 Sink 接口扩展），当前简化。
type EncryptSink struct {
	Inner    Sink
	Keys     KeyProvider
	Logger   *zap.Logger
	// IncludeHitsPlaintext 命中规则 ID 是否保留明文（推荐 true：监控面板 / 告警依赖；
	// 业务详情已加密）。
	IncludeHitsPlaintext bool
}

func NewEncryptSink(inner Sink, keys KeyProvider, logger *zap.Logger) *EncryptSink {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &EncryptSink{Inner: inner, Keys: keys, Logger: logger, IncludeHitsPlaintext: true}
}

func (s *EncryptSink) Write(ctx context.Context, a *DecisionAudit) {
	if a == nil || s.Inner == nil {
		return
	}
	enc, err := s.encryptOne(a)
	if err != nil {
		s.Logger.Warn("audit encrypt failed (fail-open: drop record)",
			zap.String("decision_id", a.DecisionID), zap.Error(err))
		return // 不能 fail to plaintext — 保护合规
	}
	s.Inner.Write(ctx, enc)
}

// encryptOne marshal 全文 → AES-GCM 加密 → 装回精简 DecisionAudit。
func (s *EncryptSink) encryptOne(a *DecisionAudit) (*DecisionAudit, error) {
	keyID := s.Keys.ActiveKeyID()
	key, err := s.Keys.DataKey(keyID)
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plain, []byte(a.DecisionID)) // AAD=decision_id 防 ciphertext 被换 id

	// 装回：仅 DecisionID / OccurredAt / Verdict / RiskScore 明文，敏感字段全清。
	out := &DecisionAudit{
		DecisionID:     a.DecisionID,
		OccurredAt:     a.OccurredAt,
		Verdict:        a.Verdict,
		RiskScore:      a.RiskScore,
		RiskLevel:      a.RiskLevel,
		EvalDurationMs: a.EvalDurationMs,
		// 元数据塞 metadata 字段（明文 / 不敏感）
		Input: AuditInput{Metadata: map[string]string{
			"enc_key_id":     keyID,
			"enc_nonce_b64":  base64.StdEncoding.EncodeToString(nonce),
			"enc_ct_b64":     base64.StdEncoding.EncodeToString(ct),
		}},
	}
	if s.IncludeHitsPlaintext {
		// 只留 rule_id（不带 detail，detail 可能含敏感金额信息）
		out.Hits = make([]AuditHit, 0, len(a.Hits))
		for _, h := range a.Hits {
			out.Hits = append(out.Hits, AuditHit{
				RuleID:   h.RuleID,
				RuleName: h.RuleName,
				Decision: h.Decision,
			})
		}
	}
	return out, nil
}

// Decrypt 反向：从 inner sink 拿到的精简 DecisionAudit + 元数据 → 还原原始 audit。
// replay CLI / 客服查询面板用。
func Decrypt(enc *DecisionAudit, keys KeyProvider) (*DecisionAudit, error) {
	if enc == nil || enc.Input.Metadata == nil {
		return nil, errors.New("audit decrypt: missing metadata envelope")
	}
	keyID := enc.Input.Metadata["enc_key_id"]
	nonceB64 := enc.Input.Metadata["enc_nonce_b64"]
	ctB64 := enc.Input.Metadata["enc_ct_b64"]
	if keyID == "" || nonceB64 == "" || ctB64 == "" {
		return nil, errors.New("audit decrypt: incomplete envelope")
	}
	key, err := keys.DataKey(keyID)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(ctB64)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ct, []byte(enc.DecisionID))
	if err != nil {
		return nil, fmt.Errorf("audit decrypt: GCM open: %w", err)
	}
	var orig DecisionAudit
	if err := json.Unmarshal(plain, &orig); err != nil {
		return nil, err
	}
	return &orig, nil
}
