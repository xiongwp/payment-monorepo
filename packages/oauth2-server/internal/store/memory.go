// Package store — Client 持久化抽象。
//
// 当前实现:
//   - MemoryStore: in-memory map (单机 dev / 单实例 prod)
//
// 后续可扩展:
//   - MySQLStore: 多实例共享 (HA prod 必须)
//   - RedisStore: 配 TTL + revocation 列表
//
// Schema 见 database/init/01_schema.sql:
//   - clients 表 (client_id, secret_hash, owner_type, ...)
//   - revoked_tokens 表 (jti, expires_at) — Introspect 时查

package store

import (
	"errors"
	"strings"
	"sync"
	"time"

	"reconcile-system/packages/oauth2-server/internal/domain"

	"golang.org/x/crypto/bcrypt"
)

// ErrClientNotFound 客户端不存在。
var ErrClientNotFound = errors.New("client not found")

// ErrInvalidSecret secret 校验失败。
var ErrInvalidSecret = errors.New("invalid client_secret")

// ErrClientSuspended 客户端被冻结 / 撤销。
var ErrClientSuspended = errors.New("client suspended or revoked")

// ErrClientExpired 客户端凭据已过期。
var ErrClientExpired = errors.New("client credentials expired")

// ErrIPNotAllowed 当前 IP 不在白名单。
var ErrIPNotAllowed = errors.New("source IP not in client allowlist")

// ErrScopeNotAllowed 请求 scope 超出客户端授权范围。
var ErrScopeNotAllowed = errors.New("requested scope exceeds client allowed scope")

// Store 客户端 + revocation 抽象。
type Store interface {
	GetClient(clientID string) (*domain.Client, error)
	PutClient(c *domain.Client) error
	ListClients() ([]*domain.Client, error)
	UpdateLastUsed(clientID string, t time.Time) error
	Revoke(jti string, exp time.Time) error
	IsRevoked(jti string) bool
}

// MemoryStore in-memory 实现。
type MemoryStore struct {
	mu       sync.RWMutex
	clients  map[string]*domain.Client
	revoked  map[string]time.Time // jti -> exp
}

// NewMemoryStore 构造。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		clients: map[string]*domain.Client{},
		revoked: map[string]time.Time{},
	}
}

// GetClient 按 client_id 查。
func (m *MemoryStore) GetClient(clientID string) (*domain.Client, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clients[clientID]
	if !ok {
		return nil, ErrClientNotFound
	}
	return c, nil
}

// PutClient 写入 / 更新。
func (m *MemoryStore) PutClient(c *domain.Client) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	c.UpdatedAt = time.Now().UTC()
	m.clients[c.ClientID] = c
	return nil
}

// ListClients 列所有客户端 (ops 后台用)。
func (m *MemoryStore) ListClients() ([]*domain.Client, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.Client, 0, len(m.clients))
	for _, c := range m.clients {
		out = append(out, c)
	}
	return out, nil
}

// UpdateLastUsed 刷新 last_used_at (异步 / best-effort)。
func (m *MemoryStore) UpdateLastUsed(clientID string, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[clientID]
	if !ok {
		return ErrClientNotFound
	}
	c.LastUsedAt = &t
	return nil
}

// Revoke 把 jti 加入黑名单 (revocation)。
func (m *MemoryStore) Revoke(jti string, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revoked[jti] = exp
	return nil
}

// IsRevoked 是否在黑名单。
func (m *MemoryStore) IsRevoked(jti string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	exp, ok := m.revoked[jti]
	if !ok {
		return false
	}
	// 过期了就清掉
	if time.Now().After(exp) {
		// 这里读锁不能 delete，等下次写锁清，影响不大
		return false
	}
	return true
}

// GCRevoked 清理已过期的 revocation。
func (m *MemoryStore) GCRevoked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	n := 0
	for jti, exp := range m.revoked {
		if now.After(exp) {
			delete(m.revoked, jti)
			n++
		}
	}
	return n
}

// ─── 高阶验证 ──────────────────────────────────────────────────────

// AuthenticateClient 完整校验: 存在 + secret + status + expiry + IP + scope。
// 返通过 scope (intersect of requested ∩ allowed)。
func AuthenticateClient(s Store, clientID, clientSecret, requestedScope, sourceIP string) (*domain.Client, string, error) {
	c, err := s.GetClient(clientID)
	if err != nil {
		return nil, "", err
	}
	// secret 校验 (bcrypt)
	if err := bcrypt.CompareHashAndPassword([]byte(c.SecretHash), []byte(clientSecret)); err != nil {
		return nil, "", ErrInvalidSecret
	}
	// status
	if c.Status != "active" {
		return nil, "", ErrClientSuspended
	}
	// expiry
	if c.ExpiresAt != nil && time.Now().After(*c.ExpiresAt) {
		return nil, "", ErrClientExpired
	}
	// IP 白名单 (空 = 不限制)
	if c.AllowedIPs != "" && sourceIP != "" {
		if !ipInList(sourceIP, c.AllowedIPs) {
			return nil, "", ErrIPNotAllowed
		}
	}
	// scope 交集
	grantedScope := intersectScopes(requestedScope, c.AllowedScopes)
	if requestedScope != "" && grantedScope == "" {
		return nil, "", ErrScopeNotAllowed
	}
	if grantedScope == "" {
		grantedScope = c.AllowedScopes // 没请求就给全部允许的
	}
	return c, grantedScope, nil
}

func ipInList(ip, csv string) bool {
	for _, item := range strings.Split(csv, ",") {
		if strings.TrimSpace(item) == ip {
			return true
		}
	}
	return false
}

func intersectScopes(requested, allowed string) string {
	if requested == "" {
		return ""
	}
	allowedSet := map[string]struct{}{}
	for _, s := range splitScopes(allowed) {
		allowedSet[s] = struct{}{}
	}
	out := []string{}
	for _, s := range splitScopes(requested) {
		if _, ok := allowedSet[s]; ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

func splitScopes(s string) []string {
	out := []string{}
	for _, t := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	}) {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// HashSecret bcrypt 哈希 (创建客户端时调)。
func HashSecret(secret string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}
