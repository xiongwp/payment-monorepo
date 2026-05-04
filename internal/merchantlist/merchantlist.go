// Package merchantlist 商户专属 allow / block 名单。
//
// 跟全局 store.Blacklist 区别：
//   - Blacklist 是平台级，risk-manage 运营加的（防欺诈名单）
//   - merchantlist 是商户级，每个商户独立维护"我自己见过的好人 / 坏人"
//
// 用途（Stripe Radar 标配能力）：
//   - allow：明确放行的客户 / 卡 / IP（VIP / 反复退款客户但合法）→ 跳过所有规则
//   - block：商户已确认的欺诈（chargeback 客户、被盗卡、滥用 VPN）→ 直接 DENY
//
// 核心规则：allowlist 命中 = 短路 ALLOW（绕过所有 rule + score）；blocklist
// 命中 = 短路 DENY。商户能完全控制自己的"白名单 / 黑名单"。
//
// 维度（dimension）：customer / device / ip / card_fingerprint / email
// （email/card_fingerprint 这种敏感字段建议落 KMS 加密的 hash，不直接存原值）
package merchantlist

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind 名单类别。
type Kind string

const (
	KindAllow Kind = "allow"
	KindBlock Kind = "block"
)

// Entry 一条名单条目。Allow 名单 ExpiresAt 一般会设（"放行 30 天"），
// Block 名单往往 ExpiresAt=0 长期有效。
type Entry struct {
	MerchantID string    `json:"merchant_id"`
	Kind       Kind      `json:"kind"` // allow | block
	Dimension  string    `json:"dimension"`
	Value      string    `json:"value"`
	Reason     string    `json:"reason"`
	Actor      string    `json:"actor,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"` // zero = 长期
}

// Service 名单查询 / 维护。
type Service interface {
	// Contains 在 merchant 的 kind 名单里查 (dimension, value)。
	// O(1)；过期的 entry 不算命中（同时由 sweeper 清理）。
	Contains(ctx context.Context, merchantID string, kind Kind, dimension, value string) (*Entry, bool)
	Add(ctx context.Context, e Entry) error
	Remove(ctx context.Context, merchantID string, kind Kind, dimension, value string) bool
	List(ctx context.Context, merchantID string, kind Kind) []*Entry
}

// MemService 内存版（dev / 单测）。生产 PG-backed：
//
//	CREATE TABLE risk_merchant_list (
//	    merchant_id  TEXT NOT NULL,
//	    kind         TEXT NOT NULL,    -- allow | block
//	    dimension    TEXT NOT NULL,
//	    value        TEXT NOT NULL,
//	    reason       TEXT,
//	    actor        TEXT,
//	    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	    expires_at   TIMESTAMPTZ,
//	    PRIMARY KEY (merchant_id, kind, dimension, value)
//	);
type MemService struct {
	mu      sync.RWMutex
	entries map[string]*Entry // key = merchant_id|kind|dim|value
}

func NewMemService() *MemService {
	return &MemService{entries: make(map[string]*Entry)}
}

func keyOf(merchantID string, kind Kind, dim, val string) string {
	return strings.ToLower(merchantID) + "|" + string(kind) + "|" +
		strings.ToLower(dim) + "|" + strings.ToLower(strings.TrimSpace(val))
}

func (s *MemService) Add(_ context.Context, e Entry) error {
	if e.MerchantID == "" || e.Dimension == "" || e.Value == "" {
		return errors.New("merchantlist: merchant_id/dimension/value required")
	}
	if e.Kind != KindAllow && e.Kind != KindBlock {
		return errors.New("merchantlist: kind must be allow|block")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	cp := e
	s.mu.Lock()
	s.entries[keyOf(e.MerchantID, e.Kind, e.Dimension, e.Value)] = &cp
	s.mu.Unlock()
	return nil
}

func (s *MemService) Remove(_ context.Context, merchantID string, kind Kind, dim, val string) bool {
	k := keyOf(merchantID, kind, dim, val)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[k]
	delete(s.entries, k)
	return ok
}

func (s *MemService) Contains(_ context.Context, merchantID string, kind Kind, dim, val string) (*Entry, bool) {
	if merchantID == "" || val == "" {
		return nil, false
	}
	s.mu.RLock()
	e, ok := s.entries[keyOf(merchantID, kind, dim, val)]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if !e.ExpiresAt.IsZero() && time.Now().After(e.ExpiresAt) {
		// 过期，惰性清理
		s.mu.Lock()
		delete(s.entries, keyOf(merchantID, kind, dim, val))
		s.mu.Unlock()
		return nil, false
	}
	cp := *e
	return &cp, true
}

func (s *MemService) List(_ context.Context, merchantID string, kind Kind) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]*Entry, 0, 16)
	for _, e := range s.entries {
		if e.MerchantID != merchantID {
			continue
		}
		if kind != "" && e.Kind != kind {
			continue
		}
		if !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt) {
			continue
		}
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}
