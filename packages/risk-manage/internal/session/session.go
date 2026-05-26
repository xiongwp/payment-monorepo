// Package session 持久化 web-sdk 上报的 fingerprint + behavior 快照。
//
// 流程：
//
//	checkout 页面加载   → POST /v1/risk/session            返回 session_id
//	checkout 提交       → POST /v1/risk/session/finalize  附行为快照
//	支付下单（merchant）→ PI.metadata.risk_session_id = id
//	risk-manage Screen  → 用 id 查 store → 富化 TxnContext
//
// 接口分两层：
//   - Store：纯持久化，session_id → Snapshot
//   - HTTP handler（在 metrics 包注册）：接 SDK POST + 提供 GET 给内部 service
//
// 当前 Mem 实现是内存 + TTL，生产替换为 Redis（HSET + EXPIRE）。
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Snapshot 端 SDK 上报的全部字段聚合。Fingerprint 在创建时填；Behavior 在
// finalize 时覆盖（fingerprint 不变）。
type Snapshot struct {
	SessionID  string    `json:"session_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	SDKVersion string    `json:"sdk_version,omitempty"`
	// MerchantID 来自请求 X-Risk-Merchant-Id（已签名校验过）；DSR Erase 用 customer_id
	// 检索 + merchant 隔离；非常关键，不要让前端在 body 里直接传以防越权。
	MerchantID string `json:"merchant_id,omitempty"`
	// CustomerID 可选；商户已知用户身份时可在签名请求里带，用于 GDPR Erase 索引。
	// 不带 = 这条 session 不会被按 customer 删（兼容游客 checkout）。
	CustomerID string `json:"customer_id,omitempty"`

	// Fingerprint
	// DSR / PII minimization：以下三个字段在 SDK 上报后由 NormalizeForStorage 强制
	// 哈希化（hex sha256）。原值是高基数浏览器/设备识别符（GPU 名 / UA 字符串），
	// 长期持久会被监管视为 PII（GDPR Art. 4 个人数据 — 间接识别）。哈希后既能保
	// 留指纹比对能力，又能在 DSR Erase 时一并清除。
	FingerprintHash     string `json:"fingerprintHash,omitempty"`
	CanvasFingerprint   string `json:"canvasFingerprint,omitempty"`   // 端 SDK 已 hash
	WebGLRenderer       string `json:"webglRenderer,omitempty"`       // hex hash（NormalizeForStorage）
	AudioContextHash    string `json:"audioContextHash,omitempty"`    // 端 SDK 已 hash
	ScreenWxH           string `json:"screenWxH,omitempty"`
	Timezone            string `json:"timezone,omitempty"`
	Language            string `json:"language,omitempty"`
	HardwareConcurrency int    `json:"hardwareConcurrency,omitempty"`
	Platform            string `json:"platform,omitempty"`
	UserAgent           string `json:"userAgent,omitempty"` // hex hash（NormalizeForStorage）

	// Behavior
	TimeToCheckoutMs     int64    `json:"timeToCheckoutMs,omitempty"`
	MouseMovementEntropy float64  `json:"mouseMovementEntropy,omitempty"`
	ClickIntervalMs      int      `json:"clickIntervalMs,omitempty"`
	ScrollSpeedPxPerSec  float64  `json:"scrollSpeedPxPerSec,omitempty"`
	TypingRhythmCV       float64  `json:"typingRhythmCV,omitempty"`
	KeystrokeCount       int      `json:"keystrokeCount,omitempty"`
	MouseMoves           int      `json:"mouseMoves,omitempty"`
	PastedFields         []string `json:"pastedFields,omitempty"`

	// IP 在收到 HTTP 请求时由 server 自动填（X-Forwarded-For 取一跳）
	IPAddress string `json:"ipAddress,omitempty"`
}

// Store session 持久化接口。
type Store interface {
	// Create 用 fingerprint 字段建一条 session，返回新生成的 ID。
	Create(s Snapshot) (string, error)
	// Finalize 用 behavior 字段更新已有 session；id 不存在 → ErrNotFound。
	Finalize(id string, behavior BehaviorPatch) error
	// Get 取完整 session；找不到返回 nil。
	Get(id string) *Snapshot
	// Erase 删除 customer_id 关联的所有 session（GDPR / CCPA 删除请求）。
	// customer_id 不存在 / 没有任何关联 session 都视作成功（幂等）。
	// 返回 nil = 成功；返回 error = 后端不可达（调用方应重试 / 排查）。
	Erase(ctx context.Context, customerID string) error
}

// BehaviorPatch finalize 时上报的字段子集。fingerprint 字段保持原样不动。
type BehaviorPatch struct {
	TimeToCheckoutMs     int64
	MouseMovementEntropy float64
	ClickIntervalMs      int
	ScrollSpeedPxPerSec  float64
	TypingRhythmCV       float64
	KeystrokeCount       int
	MouseMoves           int
	PastedFields         []string
}

// ErrNotFound finalize / get 时 id 不存在。
type ErrNotFound struct{ ID string }

func (e ErrNotFound) Error() string { return "session not found: " + e.ID }

// ─── MemStore ──────────────────────────────────────────────────────

const sessionTTL = 30 * time.Minute

// MemStore 内存版 Store，TTL 30min（典型 checkout 时长）。
// 生产 Redis 实现：HSET sess:<id> + EXPIRE sess:<id> 1800。
type MemStore struct {
	mu    sync.RWMutex
	items map[string]*Snapshot
	// byCustomer secondary index：customer_id → session_id set，
	// 让 DSR Erase 不用全表扫。Redis 版用 SET 同义实现。
	byCustomer map[string]map[string]struct{}
}

func NewMemStore() *MemStore {
	return &MemStore{
		items:      make(map[string]*Snapshot),
		byCustomer: make(map[string]map[string]struct{}),
	}
}

func (m *MemStore) Create(s Snapshot) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	s.SessionID = id
	s.CreatedAt = now
	s.UpdatedAt = now
	NormalizeForStorage(&s)
	m.mu.Lock()
	m.items[id] = &s
	if s.CustomerID != "" {
		set, ok := m.byCustomer[s.CustomerID]
		if !ok {
			set = make(map[string]struct{})
			m.byCustomer[s.CustomerID] = set
		}
		set[id] = struct{}{}
	}
	m.mu.Unlock()
	return id, nil
}

func (m *MemStore) Finalize(id string, b BehaviorPatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.items[id]
	if !ok {
		return ErrNotFound{ID: id}
	}
	s.TimeToCheckoutMs = b.TimeToCheckoutMs
	s.MouseMovementEntropy = b.MouseMovementEntropy
	s.ClickIntervalMs = b.ClickIntervalMs
	s.ScrollSpeedPxPerSec = b.ScrollSpeedPxPerSec
	s.TypingRhythmCV = b.TypingRhythmCV
	s.KeystrokeCount = b.KeystrokeCount
	s.MouseMoves = b.MouseMoves
	s.PastedFields = b.PastedFields
	s.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *MemStore) Get(id string) *Snapshot {
	m.mu.RLock()
	s, ok := m.items[id]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Since(s.CreatedAt) > sessionTTL {
		m.mu.Lock()
		delete(m.items, id)
		if s.CustomerID != "" {
			if set, ok := m.byCustomer[s.CustomerID]; ok {
				delete(set, id)
				if len(set) == 0 {
					delete(m.byCustomer, s.CustomerID)
				}
			}
		}
		m.mu.Unlock()
		return nil
	}
	cp := *s
	return &cp
}

// Erase 删除 customer_id 关联的所有 session（GDPR / CCPA right-to-erasure）。
// 空 customer_id 视作 no-op；找不到也返 nil（幂等保证调用方安全重试）。
func (m *MemStore) Erase(_ context.Context, customerID string) error {
	if customerID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.byCustomer[customerID]
	if !ok {
		return nil
	}
	for id := range set {
		delete(m.items, id)
	}
	delete(m.byCustomer, customerID)
	return nil
}

// NormalizeForStorage 把 Snapshot 里仍为原值的高基数 PII 字段哈希化。
//
// 规则：identifies-like-hex 直接保留；否则 sha256(value) 转 hex。这样运维既不需要
// 强制升级 SDK，又确保 store 里不再有"裸 UA / 裸 GPU 名"。
//
// 影响范围（DSR / PII minimization）：
//   - WebGLRenderer "ANGLE (Intel UHD Graphics 630)" → 哈希
//   - UserAgent "Mozilla/5.0 ..."                   → 哈希
//
// CanvasFingerprint / AudioContextHash 端 SDK 上报时已经是 hash；FingerprintHash 同。
// IP / ScreenWxH / Timezone / Language / Platform / HwConcurrency 在风控信号意义上
// 比 raw UA 价值更高且基数低，未列入强制哈希范围（合规层视风控基本最小集）。
func NormalizeForStorage(s *Snapshot) {
	if s == nil {
		return
	}
	s.WebGLRenderer = ensureHexHash(s.WebGLRenderer)
	s.UserAgent = ensureHexHash(s.UserAgent)
}

// ensureHexHash：已经是 64 hex chars（sha256）直接返；否则 sha256 hex 化。
// 空串保留空串以维持 omitempty 语义。
func ensureHexHash(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) == 64 && isHex(v) {
		return strings.ToLower(v)
	}
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func isHex(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// newID 16-byte crypto/rand → 32 hex 字符。和 audit decision_id 同款。
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
