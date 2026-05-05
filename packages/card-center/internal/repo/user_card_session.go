// user_card_session.go: card-center HTTPS 入口的 Bearer 会话仓储（**备选方案**）。
//
// ⚠️ 当前生产路径走 internal/httpsauth/verifier_user_merchant.go：
//
//	浏览器 cookie/Bearer jwt → card-center HTTPS → user-merchant-core.IntrospectToken (mTLS gRPC)
//	→ 命中即注 user_id 到 ctx
//
// 这种"introspection"路径不需要 card-center 自己签发 / 存 session，所以本文件
// 当前**没有被 main.go 装配**。保留是因为在某些场景（前端 SDK 长会话、与 jwt 解耦）
// 这种 stateful session 模型仍有价值。
//
// 表 user_card_session 在 metadb（不分片：量小、TTL 短、只按 session_hash 查）。
//
// 调用纪律（如启用）：
//   - Issue：仅由 mTLS gRPC 内部接口调（CN 白名单 = api-gateway）
//   - Validate：HTTPS 入口的 auth middleware 调；命中即返 user_id，未命中返 ErrSessionInvalid
//   - MarkUsed：tokenize scope 一次性，绑卡成功后立即标 used_count=1，下次 Validate 拒
//   - Revoke：用户登出 / 风控触发时显式调
package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// UserCardSessionRow user_card_session 表行。
//
// **不**存 session token 本身，只存 sha256 摘要。这样 DB 泄露 attacker 也拿不到能用的 ucs_xxx。
type UserCardSessionRow struct {
	ID          int64      `gorm:"column:id;primaryKey;autoIncrement"`
	SessionHash string     `gorm:"column:session_hash"`
	UserID      int64      `gorm:"column:user_id"`
	Scope       string     `gorm:"column:scope"` // tokenize / list / delete
	IssuedTo    string     `gorm:"column:issued_to"`
	ClientIP    string     `gorm:"column:client_ip"`
	ExpiresAt   time.Time  `gorm:"column:expires_at"`
	UsedCount   int        `gorm:"column:used_count"`
	RevokedAt   *time.Time `gorm:"column:revoked_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
}

// 错误集
var (
	// ErrSessionInvalid 会话不存在 / hash 不匹配
	ErrSessionInvalid = errors.New("repo: user_card_session invalid")
	// ErrSessionExpired 已过期
	ErrSessionExpired = errors.New("repo: user_card_session expired")
	// ErrSessionRevoked 已被撤销
	ErrSessionRevoked = errors.New("repo: user_card_session revoked")
	// ErrSessionUsedUp tokenize scope 已使用过（单次）
	ErrSessionUsedUp = errors.New("repo: user_card_session already used")
	// ErrSessionScopeMismatch 持有的 session scope 不匹配本次操作
	ErrSessionScopeMismatch = errors.New("repo: user_card_session scope mismatch")
)

// UserCardSessionRepo
type UserCardSessionRepo interface {
	// Insert 新签发一个 session。session_hash 唯一约束防同一 token 重复入库。
	Insert(ctx context.Context, row *UserCardSessionRow) error
	// GetByHash 按 sha256 查 session（HTTPS handler 校验时用）。
	GetByHash(ctx context.Context, sessionHash string) (*UserCardSessionRow, error)
	// IncrementUsed used_count++（仅 tokenize scope 用，原子 UPDATE 保证单次）
	IncrementUsedAtomic(ctx context.Context, sessionHash string, maxUsed int) error
	// Revoke 标 revoked_at=now（用户登出 / 风控触发）
	Revoke(ctx context.Context, sessionHash, reason string) error
	// CleanupExpired cron 清掉 expires_at < now - 1h 的行（tamper-evident audit_log 留 7 年，session 只留 1h 兜底）
	CleanupExpired(ctx context.Context) (int64, error)
}

const tblUserCardSession = "user_card_session"

type userCardSessionRepo struct {
	mgr *Manager
}

// NewUserCardSessionRepo 构造（用 metadb）
func NewUserCardSessionRepo(mgr *Manager) UserCardSessionRepo {
	return &userCardSessionRepo{mgr: mgr}
}

func (r *userCardSessionRepo) db(ctx context.Context) *gorm.DB {
	return r.mgr.Meta().WithContext(ctx).Table(tblUserCardSession)
}

func (r *userCardSessionRepo) Insert(ctx context.Context, row *UserCardSessionRow) error {
	if row.SessionHash == "" || row.UserID == 0 || row.Scope == "" {
		return fmt.Errorf("Insert: session_hash / user_id / scope required")
	}
	if row.ExpiresAt.IsZero() {
		return fmt.Errorf("Insert: expires_at required")
	}
	return r.db(ctx).Create(row).Error
}

func (r *userCardSessionRepo) GetByHash(ctx context.Context, sessionHash string) (*UserCardSessionRow, error) {
	var row UserCardSessionRow
	err := r.db(ctx).Where("session_hash = ?", sessionHash).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSessionInvalid
	}
	return &row, err
}

// IncrementUsedAtomic 原子 UPDATE：used_count < maxUsed 才能 +1。
// tokenize scope 用 maxUsed=1 实现"单次"。
func (r *userCardSessionRepo) IncrementUsedAtomic(ctx context.Context, sessionHash string, maxUsed int) error {
	res := r.db(ctx).
		Where("session_hash = ? AND used_count < ?", sessionHash, maxUsed).
		Update("used_count", gorm.Expr("used_count + 1"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 行存在但 used_count 已达上限 → ErrSessionUsedUp；行不存在 → ErrSessionInvalid
		// 这里粗一档返 used up（GetByHash 已经保证行存在的前置）
		return ErrSessionUsedUp
	}
	return nil
}

func (r *userCardSessionRepo) Revoke(ctx context.Context, sessionHash, reason string) error {
	_ = reason // reason 进 audit_log，不进 session 行
	now := time.Now()
	res := r.db(ctx).
		Where("session_hash = ? AND revoked_at IS NULL", sessionHash).
		Update("revoked_at", &now)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrSessionInvalid
	}
	return nil
}

func (r *userCardSessionRepo) CleanupExpired(ctx context.Context) (int64, error) {
	cutoff := time.Now().Add(-1 * time.Hour)
	res := r.db(ctx).Where("expires_at < ?", cutoff).Delete(&UserCardSessionRow{})
	return res.RowsAffected, res.Error
}
