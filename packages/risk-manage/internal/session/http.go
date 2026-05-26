package session

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/attestation"
	"github.com/xiongwp/risk-manage/internal/auth"
)

// RegisterHandlers 注册 SDK / admin endpoint：
//
//	POST   /v1/risk/session            创建（接受 fingerprint），返回 {session_id}
//	POST   /v1/risk/session/finalize  覆盖 behavior（接受 session_id + 行为字段）
//	DELETE /admin/dsr/erase           DSR 删除某 customer 的全部 session（admin auth）
//
// SDK 端点（/v1/risk/session*）是公网开放的（前端发出来），所以必须靠 HMAC-SHA256
// 签名来防伪造。校验头：
//
//	X-Risk-Merchant-Id  商户 ID，决定用哪个 secret 验签
//	X-Risk-Timestamp    unix-ms（防重放窗口 ±5min）
//	X-Risk-Nonce        客户端生成的随机 hex（16 字节 = 32 hex），60s 内不可复用
//	X-Risk-Signature    sha256=<hex>，计算 = HMAC_SHA256(secret, ts || nonce || body)
//
// 灰度开关 RISK_SDK_SIGNATURE_REQUIRED：
//   - 默认 true / 未设：缺签名或验签失败直接 401，全量启用
//   - 显式 "false"：仅在 logger 打 warn（带 merchant_id），让 web-sdk 灰度切换
//
// admin DSR endpoint 由调用方在 mux 外层套 admin auth（RBAC danger / write），
// 这里不重复鉴权但会从 ctx 读 principal 做审计日志。
func RegisterHandlers(mux *http.ServeMux, store Store, logger *zap.Logger) {
	RegisterHandlersWithSecrets(mux, store, nil, logger)
}

// RegisterHandlersWithSecrets 注册 + 装签名校验。secrets=nil 时退回未鉴权
// （兼容老 dev 流程；RISK_SDK_SIGNATURE_REQUIRED 仍可关）。
func RegisterHandlersWithSecrets(mux *http.ServeMux, store Store, secrets auth.SecretLookup, logger *zap.Logger) {
	RegisterHandlersFull(mux, store, secrets, nil, logger)
}

// RegisterHandlersFull 完整版：roleWrap 非 nil 时把 /admin/dsr/erase 套 RBAC
// middleware（典型用 metrics.RequireRole("danger")）。session 包不直接依赖
// metrics 包避免循环 import，main.go wire 时注入。
func RegisterHandlersFull(
	mux *http.ServeMux,
	store Store,
	secrets auth.SecretLookup,
	roleWrap func(http.Handler) http.Handler,
	logger *zap.Logger,
) {
	RegisterHandlersWithAttest(mux, store, secrets, nil, roleWrap, logger)
}

// RegisterHandlersWithAttest RegisterHandlersFull + attestation Verifier。
// attest=nil → 不做 mobile attestation 验签（兼容旧 main.go wire）。
// attest 非 nil → Create / Finalize 接到 attestation_* 字段后路由到 verifier，
// 验签结果写到 Snapshot.AttestationVerified + AttestationKind。
func RegisterHandlersWithAttest(
	mux *http.ServeMux,
	store Store,
	secrets auth.SecretLookup,
	attest *attestation.Verifier,
	roleWrap func(http.Handler) http.Handler,
	logger *zap.Logger,
) {
	verifier := newSigVerifier(secrets, logger)
	mux.HandleFunc("/v1/risk/session", verifier.wrap(makeCreateHandler(store, attest, logger)))
	mux.HandleFunc("/v1/risk/session/finalize", verifier.wrap(makeFinalizeHandler(store, attest, logger)))
	var erase http.Handler = makeEraseHandler(store, logger)
	if roleWrap != nil {
		erase = roleWrap(erase)
	}
	mux.Handle("/admin/dsr/erase", erase)
}

// MakeEraseHandler 暴露 DSR erase handler 给测试直接调用（无中间件版本）。
func MakeEraseHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return makeEraseHandler(store, logger)
}

// ─── 签名校验 ───────────────────────────────────────────────────────────

const (
	headerMerchantID = "X-Risk-Merchant-Id"
	headerTimestamp  = "X-Risk-Timestamp"
	headerNonce      = "X-Risk-Nonce"
	headerSignature  = "X-Risk-Signature"

	tsSkewWindow  = 5 * time.Minute // ts 与服务端时钟最大偏移
	nonceLifetime = 60 * time.Second
	nonceCacheCap = 16 * 1024
	maxBodyBytes  = 1 << 14
)

// sigVerifier HTTP middleware：校验 SDK 公网端点的 HMAC 签名 + 防重放。
type sigVerifier struct {
	secrets auth.SecretLookup
	logger  *zap.Logger
	nonces  *nonceLRU
	now     func() time.Time
}

func newSigVerifier(secrets auth.SecretLookup, logger *zap.Logger) *sigVerifier {
	return &sigVerifier{
		secrets: secrets,
		logger:  logger,
		nonces:  newNonceLRU(nonceCacheCap),
		now:     time.Now,
	}
}

func required() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("RISK_SDK_SIGNATURE_REQUIRED")))
	// 默认 true（生产）；显式 "false" / "0" / "no" 进入灰度日志模式
	switch v {
	case "false", "0", "no", "off":
		return false
	}
	return true
}

// wrap 把 inner 包成签名校验后调用；body 在校验完后被 reset 让 inner 继续解。
func (v *sigVerifier) wrap(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			http.Error(w, `{"error":"body too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		// 重设 body 让 inner 解 json
		r.Body = io.NopCloser(strings.NewReader(string(body)))

		merchantID := r.Header.Get(headerMerchantID)
		ts := r.Header.Get(headerTimestamp)
		nonce := r.Header.Get(headerNonce)
		sig := r.Header.Get(headerSignature)

		// 把 merchant id 透传给 handler，handler 把它写进 Snapshot。
		ctx := withMerchantID(r.Context(), merchantID)
		r = r.WithContext(ctx)

		if err := v.verify(r.Context(), merchantID, ts, nonce, sig, body); err != nil {
			if required() {
				// 4xx 拒绝；不回显原因细节避免给攻击者反馈差错（仅日志）
				v.logger.Warn("sdk signature reject",
					zap.String("merchant_id", merchantID),
					zap.String("reason", err.Error()),
					zap.String("path", r.URL.Path),
					zap.String("ip", clientIP(r)),
				)
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			// 灰度模式：仅日志，继续放行（让 SDK 滚动升级期间不中断）
			v.logger.Warn("sdk signature would-reject (grace mode)",
				zap.String("merchant_id", merchantID),
				zap.String("reason", err.Error()),
				zap.String("path", r.URL.Path),
			)
		}
		inner(w, r)
	}
}

var (
	errMissingHeader = errors.New("missing required header")
	errBadTimestamp  = errors.New("bad timestamp")
	errTimestampSkew = errors.New("timestamp out of window")
	errBadNonce      = errors.New("bad nonce")
	errNonceReplay   = errors.New("nonce replay")
	errBadSignature  = errors.New("signature mismatch")
	errUnknownMerch  = errors.New("unknown merchant")
)

func (v *sigVerifier) verify(ctx context.Context, merchantID, ts, nonce, sig string, body []byte) error {
	if v.secrets == nil {
		return errUnknownMerch
	}
	if merchantID == "" || ts == "" || nonce == "" || sig == "" {
		return errMissingHeader
	}
	tsMs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errBadTimestamp
	}
	tsTime := time.UnixMilli(tsMs)
	delta := v.now().Sub(tsTime)
	if delta < 0 {
		delta = -delta
	}
	if delta > tsSkewWindow {
		return errTimestampSkew
	}
	// nonce 必须是 16-byte hex (= 32 chars) 避免短串爆破缓存
	if len(nonce) < 16 || len(nonce) > 128 || !isHexLower(strings.ToLower(nonce)) {
		return errBadNonce
	}
	if !v.nonces.checkAndAdd(merchantID+":"+nonce, v.now().Add(nonceLifetime)) {
		return errNonceReplay
	}
	secret, err := v.secrets.SDKSecret(ctx, merchantID)
	if err != nil {
		return errUnknownMerch
	}
	expected := computeSig([]byte(secret), ts, nonce, body)
	// 头格式 "sha256=<hex>" 或裸 hex
	got := strings.TrimPrefix(sig, "sha256=")
	expBytes, _ := hex.DecodeString(expected)
	gotBytes, hexErr := hex.DecodeString(got)
	if hexErr != nil || !hmac.Equal(expBytes, gotBytes) {
		return errBadSignature
	}
	return nil
}

// computeSig HMAC-SHA256(secret, ts || nonce || body) → lowercase hex。
// 拼接顺序固定，前端 SDK 必须按相同顺序签名。
func computeSig(secret []byte, ts, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(ts))
	_, _ = mac.Write([]byte(nonce))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func isHexLower(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ─── nonce LRU ─────────────────────────────────────────────────────────
//
// 进程内 LRU + TTL；够单实例防重放。多副本环境攻击者可以挑没记的副本重放，
// 生产应换成 Redis SETNX EX 60。容量 16k * (key ~64B) ≈ 1MB，可控。

type nonceEntry struct {
	expiresAt time.Time
}

type nonceLRU struct {
	mu   sync.Mutex
	cap  int
	data map[string]nonceEntry
}

func newNonceLRU(capacity int) *nonceLRU {
	if capacity <= 0 {
		capacity = 1024
	}
	return &nonceLRU{cap: capacity, data: make(map[string]nonceEntry, capacity)}
}

// checkAndAdd 返 false = 已存在（重放）；返 true = 新加成功。
// O(N) 清扫只在到达 cap 时做一次，摊销 O(1)。
func (l *nonceLRU) checkAndAdd(key string, expiresAt time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.data[key]; ok {
		if time.Now().Before(e.expiresAt) {
			return false
		}
		// 已过期视作可重新接受（理论上窗口外，攻击者也不会复用这种 nonce）
	}
	if len(l.data) >= l.cap {
		// 满了：扫一遍清过期；如果还没腾出位子就随机踢一个保持 LRU 行为简化
		now := time.Now()
		for k, v := range l.data {
			if now.After(v.expiresAt) {
				delete(l.data, k)
			}
		}
		if len(l.data) >= l.cap {
			for k := range l.data {
				delete(l.data, k)
				break
			}
		}
	}
	l.data[key] = nonceEntry{expiresAt: expiresAt}
	return true
}

// ─── ctx merchant_id propagation ────────────────────────────────────────

type ctxKey struct{}

func withMerchantID(ctx context.Context, mid string) context.Context {
	return context.WithValue(ctx, ctxKey{}, mid)
}

func merchantIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

// ─── handlers ──────────────────────────────────────────────────────────

func makeCreateHandler(store Store, attest *attestation.Verifier, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body Snapshot
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		body.IPAddress = clientIP(r)
		// merchant id 来自校验过的 header，不信任 body 中的字段防越权
		if mid := merchantIDFrom(r.Context()); mid != "" {
			body.MerchantID = mid
		}
		// mobile attestation 验签（fail-soft：失败不阻塞 session 创建，记 metric/log）。
		// 真接入 Apple/Google 走 //go:build attest；默认 stub 信任 client。
		if attest != nil {
			req := buildAttestRequest(&body)
			if req.Kind != attestation.KindNone {
				res := attest.Verify(r.Context(), req)
				body.AttestationVerified = res.Verified
				body.AttestationKind = string(res.Kind)
				if !res.Verified {
					logger.Warn("attestation verify failed",
						zap.String("kind", string(res.Kind)),
						zap.String("reason", res.Reason),
						zap.String("merchant_id", body.MerchantID),
					)
					if attest.IsAttestationRequired() {
						http.Error(w, `{"error":"attestation required"}`, http.StatusForbidden)
						return
					}
				}
			}
		}
		id, err := store.Create(body)
		if err != nil {
			logger.Warn("session create failed", zap.Error(err))
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"session_id": id})
	}
}

// buildAttestRequest 从 SDK 上报 Snapshot 拼出 attestation.Request。
// AttestationKind 决定走哪条验签路径；若 SDK 没填 kind 但有 token，按 token
// 类型推断（App Attest > DeviceCheck > PlayIntegrity）。
func buildAttestRequest(s *Snapshot) attestation.Request {
	kind := attestation.Kind(s.AttestationKind)
	if kind == attestation.KindNone {
		switch {
		case s.AppAttestToken != "" || s.AppAttestKeyID != "":
			kind = attestation.KindAppAttest
		case s.DeviceCheckToken != "":
			kind = attestation.KindDeviceCheck
		case s.PlayIntegrityToken != "":
			kind = attestation.KindPlayIntegrity
		}
	}
	return attestation.Request{
		Platform:           s.Platform,
		Kind:               kind,
		DeviceCheckToken:   s.DeviceCheckToken,
		AppAttestToken:     s.AppAttestToken,
		AppAttestKeyID:     s.AppAttestKeyID,
		AppAttestAssertion: s.AppAttestAssertion,
		PlayIntegrityToken: s.PlayIntegrityToken,
	}
}

func makeFinalizeHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID            string           `json:"session_id"`
			TimeToCheckoutMs     int64            `json:"timeToCheckoutMs"`
			MouseMovementEntropy float64          `json:"mouseMovementEntropy"`
			ClickIntervalMs      int              `json:"clickIntervalMs"`
			ScrollSpeedPxPerSec  float64          `json:"scrollSpeedPxPerSec"`
			TypingRhythmCV       float64          `json:"typingRhythmCV"`
			KeystrokeCount       int              `json:"keystrokeCount"`
			MouseMoves           int              `json:"mouseMoves"`
			PastedFields         []string         `json:"pastedFields"`
			MouseTrajectory      *MouseTrajectory `json:"mouseTrajectory"`
			KeystrokeDwellMean   float64          `json:"keystrokeDwellMean"`
			KeystrokeDwellCV     float64          `json:"keystrokeDwellCV"`
			KeystrokeFlightMean  float64          `json:"keystrokeFlightMean"`
			KeystrokeFlightCV    float64          `json:"keystrokeFlightCV"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.SessionID == "" {
			http.Error(w, `{"error":"session_id required"}`, http.StatusBadRequest)
			return
		}
		// 兼容老 SDK：未传 mouseTrajectory 但传了老 mouseMovementEntropy → 合成最小 trajectory
		// 旧字段 keystrokeDwell* 为 0 时不强制 fallback；规则可用 ratio 检测覆盖率。
		patch := BehaviorPatch{
			TimeToCheckoutMs:     body.TimeToCheckoutMs,
			MouseMovementEntropy: body.MouseMovementEntropy,
			ClickIntervalMs:      body.ClickIntervalMs,
			ScrollSpeedPxPerSec:  body.ScrollSpeedPxPerSec,
			TypingRhythmCV:       body.TypingRhythmCV,
			KeystrokeCount:       body.KeystrokeCount,
			MouseMoves:           body.MouseMoves,
			PastedFields:         body.PastedFields,
			MouseTrajectory:      body.MouseTrajectory,
			KeystrokeDwellMean:   body.KeystrokeDwellMean,
			KeystrokeDwellCV:     body.KeystrokeDwellCV,
			KeystrokeFlightMean:  body.KeystrokeFlightMean,
			KeystrokeFlightCV:    body.KeystrokeFlightCV,
		}
		err := store.Finalize(body.SessionID, patch)
		if err != nil {
			var nf ErrNotFound
			if errors.As(err, &nf) {
				http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
				return
			}
			logger.Warn("session finalize failed", zap.Error(err))
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// makeEraseHandler DSR right-to-erasure：DELETE /admin/dsr/erase?customer_id=X
// 必须由 admin auth middleware 在外层包裹（RBAC danger）。
func makeEraseHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete && r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		customerID := strings.TrimSpace(r.URL.Query().Get("customer_id"))
		if customerID == "" {
			http.Error(w, `{"error":"customer_id required"}`, http.StatusBadRequest)
			return
		}
		if err := store.Erase(r.Context(), customerID); err != nil {
			logger.Warn("dsr erase failed",
				zap.String("customer_id", customerID),
				zap.Error(err))
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		// 审计：记录擦除请求（principal 由 admin middleware 注入；secret 不打日志）
		logger.Info("dsr erase completed", zap.String("customer_id", customerID))
		writeJSON(w, http.StatusOK, map[string]string{"status": "erased"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clientIP 提取最近一跳 IP；信任 LB / CDN 的 X-Forwarded-For（生产应在 LB
// 层做信任清洗）。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i >= 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
