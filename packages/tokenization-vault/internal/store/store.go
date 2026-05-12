// Package store — vault 持久化.
//
// 双层 token 模型, 三类索引:
//
//   1) PK: internal_token → InternalToken record (一次性 lookup)
//   2) IDX(merchant_id, pan_hash): 用于 exchange 时 dedup (同商户同 PAN 复用 token)
//   3) IDX(network_ref.token_ref_id): VTS/MDES 推回调时反查 internal_token
//
// 加密策略:
//   - PAN 明文不存; PAN 加密后 BLOB 落 DB (AES-256-GCM, DEK envelope by KMS)
//   - PAN hash 索引列 sha256(PAN)[:32] hex (per-merchant salted 也行 — 这里全局)
//
// dev 用 MemStore; 生产 SQL: shard by sha256(internal_token)[:N] 路由到 100 个分表.

package store

import (
	"errors"
	"sync"
	"time"

	"reconcile-system/packages/tokenization-vault/internal/domain"
)

var (
	ErrNotFound = errors.New("token: not found")
	ErrConflict = errors.New("token: conflict")
)

type Store interface {
	Create(t domain.InternalToken) error
	Get(token string) (domain.InternalToken, error)

	// FindByMerchantPANHash dedup — 同商户同 PAN 直接复用 token, 不重复 provision.
	FindByMerchantPANHash(merchantID, panHash string) (domain.InternalToken, error)

	// FindByNetworkRef provider 回调 (e.g. VTS suspended cards push) 反查.
	FindByNetworkRef(provider domain.TokenProvider, tokenRefID string) (domain.InternalToken, error)

	// UpdateNetworkRef provisioning 完成后回写 NetworkRef.
	UpdateNetworkRef(token string, ref domain.NetworkRef) error

	UpdateStatus(token string, status domain.TokenStatus) error

	// 加密 PAN 单独表 (不放主 token 表, 减少非必要曝光) — Read/Write/Delete.
	GetEncryptedPAN(token string) (string, error)
	StoreEncryptedPAN(token, ciphertext string) error
	DeleteEncryptedPAN(token string) error

	// 统计
	CountByMerchant(merchantID string) (int, error)
}

// ─────────────────────────────────────────────────────────
// Memory 实现
// ─────────────────────────────────────────────────────────

type MemStore struct {
	mu          sync.RWMutex
	tokens      map[string]domain.InternalToken           // internal_token → record
	byMP        map[string]string                         // merchantID|panHash → internal_token
	byRef       map[string]string                         // provider|tokenRefID → internal_token
	encPAN      map[string]string                         // internal_token → ciphertext
}

func NewMemStore() *MemStore {
	return &MemStore{
		tokens: make(map[string]domain.InternalToken),
		byMP:   make(map[string]string),
		byRef:  make(map[string]string),
		encPAN: make(map[string]string),
	}
}

func (s *MemStore) Create(t domain.InternalToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[t.Token]; ok {
		return ErrConflict
	}
	if t.Created_at.IsZero() {
		t.Created_at = time.Now().UTC()
	}
	t.Updated_at = t.Created_at
	s.tokens[t.Token] = t
	s.byMP[t.MerchantID+"|"+t.PANHash] = t.Token
	if t.NetworkRef != nil {
		s.byRef[string(t.NetworkRef.Provider)+"|"+t.NetworkRef.TokenRefID] = t.Token
	}
	return nil
}

func (s *MemStore) Get(token string) (domain.InternalToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tokens[token]
	if !ok {
		return domain.InternalToken{}, ErrNotFound
	}
	return t, nil
}

func (s *MemStore) FindByMerchantPANHash(mid, h string) (domain.InternalToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tk, ok := s.byMP[mid+"|"+h]
	if !ok {
		return domain.InternalToken{}, ErrNotFound
	}
	return s.tokens[tk], nil
}

func (s *MemStore) FindByNetworkRef(p domain.TokenProvider, ref string) (domain.InternalToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tk, ok := s.byRef[string(p)+"|"+ref]
	if !ok {
		return domain.InternalToken{}, ErrNotFound
	}
	return s.tokens[tk], nil
}

func (s *MemStore) UpdateNetworkRef(token string, ref domain.NetworkRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[token]
	if !ok {
		return ErrNotFound
	}
	t.NetworkRef = &ref
	t.Updated_at = time.Now().UTC()
	s.tokens[token] = t
	s.byRef[string(ref.Provider)+"|"+ref.TokenRefID] = token
	return nil
}

func (s *MemStore) UpdateStatus(token string, st domain.TokenStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[token]
	if !ok {
		return ErrNotFound
	}
	t.Status = st
	t.Updated_at = time.Now().UTC()
	s.tokens[token] = t
	return nil
}

func (s *MemStore) GetEncryptedPAN(token string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.encPAN[token]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *MemStore) StoreEncryptedPAN(token, ct string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[token]; !ok {
		return ErrNotFound
	}
	s.encPAN[token] = ct
	return nil
}

func (s *MemStore) DeleteEncryptedPAN(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.encPAN, token)
	return nil
}

func (s *MemStore) CountByMerchant(mid string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, t := range s.tokens {
		if t.MerchantID == mid && t.Status == domain.TokenActive {
			n++
		}
	}
	return n, nil
}
