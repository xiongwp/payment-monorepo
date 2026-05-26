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

	// 扩展硬件信号（SDK v0.2.0+ 上报；填到 TxnContext 对应字段）
	FontHash       string  `json:"fontHash,omitempty"`
	PluginsHash    string  `json:"pluginsHash,omitempty"`
	DeviceMemory   float64 `json:"deviceMemory,omitempty"`
	PixelRatio     float64 `json:"pixelRatio,omitempty"`
	ColorDepth     int     `json:"colorDepth,omitempty"`
	TouchSupport   int     `json:"touchSupport,omitempty"`
	CodecHash      string  `json:"codecHash,omitempty"`
	ConnectionType string  `json:"connectionType,omitempty"`
	CookieEnabled  bool    `json:"cookieEnabled,omitempty"`
	DoNotTrack     string  `json:"doNotTrack,omitempty"`

	// 反自动化信号
	Webdriver           bool     `json:"webdriver,omitempty"`
	CDCGlobals          []string `json:"cdcGlobals,omitempty"`
	ChromeRuntime       bool     `json:"chromeRuntime,omitempty"`
	PermissionsMismatch bool     `json:"permissionsMismatch,omitempty"`
	BatteryPresent      bool     `json:"batteryPresent,omitempty"`
	WebRTCLocalIPs      []string `json:"webRTCLocalIPs,omitempty"`

	// Mobile attestation + 反 hook（iOS/Android SDK 上报；v0.3）
	IsJailbroken        bool   `json:"isJailbroken,omitempty"`
	IsEmulator          bool   `json:"isEmulator,omitempty"`
	FridaDetected       bool   `json:"fridaDetected,omitempty"`
	XposedDetected      bool   `json:"xposedDetected,omitempty"`
	DebuggerAttached    bool   `json:"debuggerAttached,omitempty"`
	AppAttestToken      string `json:"appAttestToken,omitempty"`     // base64 attestation object
	AppAttestKeyID      string `json:"appAttestKeyId,omitempty"`     // base64
	AppAttestAssertion  string `json:"appAttestAssertion,omitempty"` // 后续启动用 assertion
	DeviceCheckToken    string `json:"deviceCheckToken,omitempty"`   // base64
	PlayIntegrityToken  string `json:"playIntegrityToken,omitempty"` // JWE
	AttestationVerified bool   `json:"attestationVerified,omitempty"`
	AttestationKind     string `json:"attestationKind,omitempty"`

	// SDK 自身采集质量
	SignalStatus   map[string]string `json:"signalStatus,omitempty"`
	SignalCoverage *SignalCoverage   `json:"signalCoverage,omitempty"`

	// Behavior
	TimeToCheckoutMs     int64    `json:"timeToCheckoutMs,omitempty"`
	MouseMovementEntropy float64  `json:"mouseMovementEntropy,omitempty"` // 兼容老字段；等于 mouseTrajectory.trajectoryEntropy
	ClickIntervalMs      int      `json:"clickIntervalMs,omitempty"`
	ScrollSpeedPxPerSec  float64  `json:"scrollSpeedPxPerSec,omitempty"`
	TypingRhythmCV       float64  `json:"typingRhythmCV,omitempty"` // 兼容老字段；等于 keystrokeFlightCV
	KeystrokeCount       int      `json:"keystrokeCount,omitempty"`
	MouseMoves           int      `json:"mouseMoves,omitempty"`
	PastedFields         []string `json:"pastedFields,omitempty"`

	// v0.2 行为信号
	MouseTrajectory     *MouseTrajectory `json:"mouseTrajectory,omitempty"`
	KeystrokeDwellMean  float64          `json:"keystrokeDwellMean,omitempty"`
	KeystrokeDwellCV    float64          `json:"keystrokeDwellCV,omitempty"`
	KeystrokeFlightMean float64          `json:"keystrokeFlightMean,omitempty"`
	KeystrokeFlightCV   float64          `json:"keystrokeFlightCV,omitempty"`

	// IP 在收到 HTTP 请求时由 server 自动填（X-Forwarded-For 取一跳）
	IPAddress string `json:"ipAddress,omitempty"`
}

// SignalCoverage SDK 端采集质量：多少信号成功 / 总数。
// 端到端透传到 TxnContext.SignalCoverageRatio 便于规则 "<0.5 = 客户端环境异常"。
type SignalCoverage struct {
	OK    int     `json:"ok"`
	Total int     `json:"total"`
	Ratio float64 `json:"ratio"`
}

// MouseTrajectory 鼠标轨迹派生指标（newMouseBuffer.analyze 输出）。
// 不持久化原始 (x,y,t) 点（privacy + 体积）；只存聚合统计。
type MouseTrajectory struct {
	Count                int     `json:"count"`
	AvgSpeedPxPerMs      float64 `json:"avgSpeedPxPerMs"`
	SpeedVariance        float64 `json:"speedVariance"`
	AccelerationKurtosis float64 `json:"accelerationKurtosis"`
	TrajectoryEntropy    float64 `json:"trajectoryEntropy"`
	StraightnessRatio    float64 `json:"straightnessRatio"`
	PauseCount           int     `json:"pauseCount"`
	DurationMs           int64   `json:"durationMs"`
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
	MouseMovementEntropy float64 // 老字段；下面 MouseTrajectory.TrajectoryEntropy 是同源
	ClickIntervalMs      int
	ScrollSpeedPxPerSec  float64
	TypingRhythmCV       float64 // 老字段；下面 KeystrokeFlightCV 是同源
	KeystrokeCount       int
	MouseMoves           int
	PastedFields         []string

	// v0.2.0 新增
	MouseTrajectory     *MouseTrajectory
	KeystrokeDwellMean  float64
	KeystrokeDwellCV    float64
	KeystrokeFlightMean float64
	KeystrokeFlightCV   float64

	// v0.3 mobile attestation：finalize 时 SDK 可补传 token / assertion
	// （初次 Create 时 token 还没到位的情况）。空值不覆盖 Create 时已存的字段。
	PlayIntegrityToken string
	AppAttestAssertion string
	AppAttestKeyID     string
	DeviceCheckToken   string
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
	s.MouseTrajectory = b.MouseTrajectory
	s.KeystrokeDwellMean = b.KeystrokeDwellMean
	s.KeystrokeDwellCV = b.KeystrokeDwellCV
	s.KeystrokeFlightMean = b.KeystrokeFlightMean
	s.KeystrokeFlightCV = b.KeystrokeFlightCV
	// attestation：finalize 时 SDK 可能补传（Create 那次 token 还在拉）；
	// 非空才覆盖避免清掉 Create 时已写入的值。
	if b.PlayIntegrityToken != "" {
		s.PlayIntegrityToken = b.PlayIntegrityToken
	}
	if b.AppAttestAssertion != "" {
		s.AppAttestAssertion = b.AppAttestAssertion
	}
	if b.AppAttestKeyID != "" {
		s.AppAttestKeyID = b.AppAttestKeyID
	}
	if b.DeviceCheckToken != "" {
		s.DeviceCheckToken = b.DeviceCheckToken
	}
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
