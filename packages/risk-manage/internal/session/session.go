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
	"crypto/rand"
	"encoding/hex"
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

	// Fingerprint
	FingerprintHash     string `json:"fingerprintHash,omitempty"`
	CanvasFingerprint   string `json:"canvasFingerprint,omitempty"`
	WebGLRenderer       string `json:"webglRenderer,omitempty"`
	AudioContextHash    string `json:"audioContextHash,omitempty"`
	ScreenWxH           string `json:"screenWxH,omitempty"`
	Timezone            string `json:"timezone,omitempty"`
	Language            string `json:"language,omitempty"`
	HardwareConcurrency int    `json:"hardwareConcurrency,omitempty"`
	Platform            string `json:"platform,omitempty"`
	UserAgent           string `json:"userAgent,omitempty"`

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
}

func NewMemStore() *MemStore {
	return &MemStore{items: make(map[string]*Snapshot)}
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
	m.mu.Lock()
	m.items[id] = &s
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
		m.mu.Unlock()
		return nil
	}
	cp := *s
	return &cp
}

// newID 16-byte crypto/rand → 32 hex 字符。和 audit decision_id 同款。
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
