package domain

import (
	"errors"
	"time"
)

// UserCard 用户存卡引用，**不含** PAN / CVV / 任何敏感数据。
//
// 真实 PAN 在 card-center 服务的 KMS-encrypted stored_token 里；本表只存：
//   - StoredToken: card-center 颁发的长期 token（用作支付时传给 card-center 派生支付 token）
//   - TokenHash: sha256(StoredToken)，唯一索引防同 token 重复入库
//   - MaskedPAN: BIN+last4，给前端展示
//   - 其它业务元数据（network / 过期月年 / 持卡人姓名）
//
// 删除：soft delete (Status=deleted, DeletedAt 设值)。物理失效靠 card-center 那边
// 的 KMS key rotation；本表标 deleted 后业务侧自然不再用此 token。
type UserCard struct {
	ID          int64      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID      int64      `gorm:"column:user_id"`
	StoredToken string     `gorm:"column:stored_token;type:varchar(1024)"`
	TokenHash   string     `gorm:"column:token_hash;type:char(64);uniqueIndex"`
	MaskedPAN   string     `gorm:"column:masked_pan;type:varchar(20)"`
	Network     string     `gorm:"column:network;type:varchar(16)"`
	ExpMonth    int        `gorm:"column:exp_month"`
	ExpYear     int        `gorm:"column:exp_year"`
	HolderName  string     `gorm:"column:holder_name;type:varchar(64)"`
	IsDefault   bool       `gorm:"column:is_default"`
	Status      UserCardStatus `gorm:"column:status;type:varchar(16);default:'active'"`
	CreatedAt   time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt   *time.Time `gorm:"column:deleted_at"`
}

func (UserCard) TableName() string { return "user_card" }

// UserCardStatus 状态枚举
type UserCardStatus string

const (
	UserCardActive  UserCardStatus = "active"
	UserCardDeleted UserCardStatus = "deleted"
	UserCardExpired UserCardStatus = "expired"
)

// 错误
var (
	ErrUserCardNotFound = errors.New("user card not found")
	ErrUserCardExpired  = errors.New("user card expired")
	ErrUserCardDeleted  = errors.New("user card deleted")
)

// IsActive 业务可用 = active 且未过期 + 未软删
func (c *UserCard) IsActive(now time.Time) bool {
	if c.Status != UserCardActive {
		return false
	}
	if c.DeletedAt != nil {
		return false
	}
	// exp 月份末日 23:59:59 仍可用；超过则 expired
	if c.ExpYear < now.Year() {
		return false
	}
	if c.ExpYear == now.Year() && c.ExpMonth < int(now.Month()) {
		return false
	}
	return true
}
