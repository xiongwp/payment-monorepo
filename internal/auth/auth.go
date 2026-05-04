// Package auth 商户级 API key + 调用方 principal。
//
// 商业化要求：
//   - 每个商户拿独立 API key 调 Screen，不能看到其他商户的 audit / review / outcome
//   - 内部服务（payment-core）走 ServiceAccount 全权限（合并到 ScopeInternal）
//   - admin 后台 / dispute 系统走 ScopeAdmin（看全平台但不能假冒商户调 Screen）
//
// API key 格式参照 Stripe：
//
//	rsk_live_<merchant_id>_<random>      生产，绑定 merchant
//	rsk_test_<merchant_id>_<random>      测试沙箱
//	rsk_admin_<random>                   admin 全平台
//	rsk_internal_<random>                payment-core / dispute 这类内部服务
//
// 存储侧：plaintext 只在创建时返回一次给商户；DB 只存 sha256 哈希（Stripe-style
// API key 不可枚举不可还原；轮换 = 创建新 key + revoke 老 key，不是 rehash）。
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

// Scope 调用方角色。
type Scope int

const (
	// ScopeUnknown 未鉴权 / 无 principal。Screen / admin 都拒。
	ScopeUnknown Scope = 0
	// ScopeMerchant 商户专属。Screen 调用 req.MerchantID 必须等于 Principal.MerchantID；
	// admin 端点只能看自己商户的数据。
	ScopeMerchant Scope = 1
	// ScopeAdmin 平台运营 / dispute system。所有 admin 端点可读写；不允许假冒
	// 商户调 Screen（req.MerchantID 必须由调用方填，不会被自动注入）。
	ScopeAdmin Scope = 2
	// ScopeInternal 内部服务账号（payment-core / accounting）。Screen 全权；
	// req.MerchantID 由 caller 填写，不强制匹配。
	ScopeInternal Scope = 3
)

func (s Scope) String() string {
	switch s {
	case ScopeMerchant:
		return "merchant"
	case ScopeAdmin:
		return "admin"
	case ScopeInternal:
		return "internal"
	}
	return "unknown"
}

// Principal 当前请求的调用方身份。从 API key 解析 + ctx 透传。
type Principal struct {
	Scope      Scope
	MerchantID string // 仅 ScopeMerchant 下非空
	KeyID      string // 给 audit / 限流用的稳定 ID（key 哈希前缀）
	// IsTest 测试 / 沙箱模式 key（rsk_test_...）。Sandbox.Detect 凭这个字段
	// 决定要不要响应 risk_test_* 触发器；生产 key 永远 false 防滥用。
	IsTest bool
	// CreatedAt key 创建时间；做"长期不轮换"告警时用。
	CreatedAt time.Time
	// AdminRole 仅 admin 路径用：read / write / danger。空 = legacy admin
	// （兼容旧 AuthTokens 配置 — 默认 = "danger" 保留旧行为）。
	// - read   只能 GET（看列表 / 状态）
	// - write  能改业务数据（review decide / outcome record）
	// - danger 能改风控配置（rule edit / ML override / chain verify）
	AdminRole string
}

// HasRole admin role 包含关系：danger 包含 write 包含 read。dev 模式
// (Principal nil) 视作 danger。
func (p *Principal) HasRole(required string) bool {
	if p == nil || p.AdminRole == "" {
		// legacy 兼容：没设 role 等于 danger（向后兼容；生产 token 应该
		// 显式设 role）
		return true
	}
	switch required {
	case "read":
		return p.AdminRole == "read" || p.AdminRole == "write" || p.AdminRole == "danger"
	case "write":
		return p.AdminRole == "write" || p.AdminRole == "danger"
	case "danger":
		return p.AdminRole == "danger"
	default:
		return false
	}
}

var (
	// ErrInvalidKey API key 不存在 / 已撤销 / 格式错。
	ErrInvalidKey = errors.New("auth: invalid api key")
	// ErrMerchantMismatch ScopeMerchant 但请求里的 merchant_id 跟 principal 不匹配。
	ErrMerchantMismatch = errors.New("auth: merchant_id mismatch")
)

// APIKeyStore API key → Principal 解析。生产 PG-backed。
type APIKeyStore interface {
	// Lookup token 是 plaintext API key（格式见包注释）。返回 Principal，找不到 / revoked 返回 ErrInvalidKey。
	Lookup(ctx context.Context, token string) (*Principal, error)
}

// HashKey 把 plaintext key 转成 sha256-hex（64 字符）。存储 / 比较都走它。
// 注意：API key 是 high-entropy 随机串（32+ bytes），sha256 已经够用，无需 bcrypt /
// argon2（那俩防字典攻击；API key 不可枚举所以省掉计算成本）。
func HashKey(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// MemAPIKeyStore 内存版（dev / 单测）。生产换 PGAPIKeyStore（//go:build pg 范式）。
type MemAPIKeyStore struct {
	mu   sync.RWMutex
	keys map[string]*Principal // key = sha256(token)
}

func NewMemAPIKeyStore() *MemAPIKeyStore {
	return &MemAPIKeyStore{keys: make(map[string]*Principal)}
}

// Add 注册一条 (token, principal)。token 是 plaintext，会被 hash 后存。
func (s *MemAPIKeyStore) Add(token string, p Principal) {
	if p.KeyID == "" {
		p.KeyID = HashKey(token)[:12]
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[HashKey(token)] = &p
}

// Revoke 删除一条 key（plaintext）。
func (s *MemAPIKeyStore) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, HashKey(token))
}

// Lookup 实现 APIKeyStore。
func (s *MemAPIKeyStore) Lookup(_ context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrInvalidKey
	}
	hashed := HashKey(token)
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 用 ConstantTimeCompare 抵御计时攻击（虽然 map[string] 本来就是 hash bucket
	// 查找；这里再加一层是 best practice）
	for k, p := range s.keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(hashed)) == 1 {
			cp := *p
			return &cp, nil
		}
	}
	return nil, ErrInvalidKey
}

// ParseKeyPrefix 从 plaintext token 解析格式信息：rsk_<env>_<scope_or_merchant>_...
// 用于 logs / metrics labeling，不做 auth 决策（auth 全靠 store 查）。
func ParseKeyPrefix(token string) (env, prefix string) {
	parts := strings.SplitN(token, "_", 4)
	if len(parts) < 3 || parts[0] != "rsk" {
		return "", ""
	}
	return parts[1], parts[2]
}

// ── ctx propagation ──────────────────────────────────────────────────

type ctxKey struct{}

// WithPrincipal 把 Principal 塞进 ctx，让下游 handler 拿。
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom 从 ctx 拿 Principal。没有 = 未鉴权（dev mode 时是允许的）。
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	if !ok || p == nil {
		return nil, false
	}
	return p, true
}

// RequireMerchantMatch 如果 principal 是 ScopeMerchant，校验 claimedMerchantID 跟
// principal 绑定的 merchant_id 一致；其他 scope 直接放行。
//
// 用法（在 Screen handler 里）：
//
//	if err := auth.RequireMerchantMatch(ctx, req.MerchantID); err != nil {
//	    return nil, status.Error(codes.PermissionDenied, err.Error())
//	}
func RequireMerchantMatch(ctx context.Context, claimedMerchantID string) error {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		// 没 principal = dev 模式 / 未启用鉴权；上层统一放行
		return nil
	}
	if p.Scope != ScopeMerchant {
		return nil
	}
	if claimedMerchantID == "" {
		// 商户 key 但没填 merchant_id → 自动填，不算错
		return nil
	}
	if claimedMerchantID != p.MerchantID {
		return ErrMerchantMismatch
	}
	return nil
}

// FillMerchantID 给 Screen 类调用兜底：principal 是 ScopeMerchant 时，
// 把 merchant_id 自动注入（避免商户每次都要重复传）。
// 调用前应该先 RequireMerchantMatch 防止商户用一个 key 调多个 merchant_id。
func FillMerchantID(ctx context.Context, claimed string) string {
	if claimed != "" {
		return claimed
	}
	p, ok := PrincipalFrom(ctx)
	if ok && p.Scope == ScopeMerchant {
		return p.MerchantID
	}
	return claimed
}

// AllowedMerchant 对 admin 端点用：principal 是 ScopeMerchant 时只允许查看
// 自己的 merchant；admin / internal scope 通过 prefer="" 表示"全部"。
//
// 用法（review / outcome / audit list）：
//
//	mid := auth.AllowedMerchant(ctx, query.Get("merchant_id"))
//	// 商户 key → 强制覆盖；admin → 用 query 参数
func AllowedMerchant(ctx context.Context, requestedMerchantID string) string {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return requestedMerchantID
	}
	if p.Scope == ScopeMerchant {
		return p.MerchantID // 强制覆盖，无视 caller 想看谁
	}
	return requestedMerchantID
}
