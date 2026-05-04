// Package idgen 只保留本服务的 biz_tag 常量 + 一个薄包装构造；leaf segment
// 算法本体搬到 pkg/idgen，其它服务可直接 replace 复用。
package idgen

import (
	"go.uber.org/zap"
	"gorm.io/gorm"

	pkgidgen "github.com/xiongwp/user-merchant-core/pkg/idgen"
)

// 业务标签（user-merchant-core 专属）
const (
	BizTagMerchant    = "user_merchant.merchant"
	BizTagKYCDocument = "user_merchant.kyc_document"
	BizTagUser        = "user_merchant.user"
)

// IDGenerator 类型别名，保持调用方 import 路径稳定。
type IDGenerator = pkgidgen.IDGenerator

// LeafAlloc 类型别名。
type LeafAlloc = pkgidgen.LeafAlloc

// New 构造并注册本服务的 biz tag。
//
// User id 段从 1e8 开始：accounting-system 把 [1e8, 9e8) 保留给 user owner_id，
// 低于 1e8 视作 platform/system 账户（CreatePlatformAccount 才能开）。Register
// 用 FirstOrCreate 只在 leaf_alloc 缺行时插入，所以从 1e8 起的旧库需要
// migration 把 max_id 抬到 1e8（见 004_users.sql 末尾 UPDATE）。
func New(metaDB *gorm.DB, logger *zap.Logger) (IDGenerator, error) {
	return pkgidgen.New(metaDB, logger, []pkgidgen.BizTag{
		{Name: BizTagMerchant, Desc: "Merchant id"},
		{Name: BizTagKYCDocument, Desc: "Merchant KYC document id"},
		{Name: BizTagUser, Desc: "User id", InitMaxID: 100_000_000},
	})
}
