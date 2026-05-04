package channel

import "context"

// PasswordHashStore 支付密码哈希的下游查询接口。
//
// 典型实现：对接用户中心 / 账户系统，按 customer_id 取 SHA256 / bcrypt 哈希。
type PasswordHashStore interface {
	GetPasswordHash(ctx context.Context, customerID string) (algorithm string, hash string, err error)
}
