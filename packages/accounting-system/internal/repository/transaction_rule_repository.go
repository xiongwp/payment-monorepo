package repository

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// TransactionRuleRepository 交易规则仓储（查询全局配置表）
//
// 替代了之前的 5 分钟 TTL 缓存方案：现在 transaction_rule + account_type_info
// 启动时一次性 load 进 immutable map，运行期 0 次 DB 查询。
// 配置变更后通过 POST /admin/reload/transaction-rules 显式触发 Reload。
//
// 为什么去掉 TTL：
//   - TTL 缓存有 1-5 min 的"配置已改但缓存未失效"窗口，发生概率低但出错严重（
//     方向写反 → 资金错位）。
//   - 显式 reload 由 admin-web 在改完规则后立即扇出，0 数据窗口。
//   - 启动时全表 load 避免冷启 N+1（之前 cache miss 才查 DB；新流量打来时多个
//     goroutine 撞同一 key 同时查 DB）。
type TransactionRuleRepository interface {
	GetRulesByProductAndEvent(ctx context.Context, productCode, eventCode string) ([]*model.TransactionRule, error)

	GetAccountTypeInfo(ctx context.Context, accountTypeCode string) (*model.AccountTypeInfo, error)

	// ListAccountTypes 返回 account_type_info 全部记录（按 owner_type 升序）。
	ListAccountTypes(ctx context.Context) ([]*model.AccountTypeInfo, error)

	// ListRulesByProduct 列指定 product_code 下所有规则; productCode 空 → 全部.
	// 供 admin /admin/transaction-rules 用 (designer event_code 下拉).
	ListRulesByProduct(ctx context.Context, productCode string) ([]*model.TransactionRule, error)

	// Reload 重新拉取 transaction_rule + account_type_info 全表，原子替换内存快照。
	// 启动时调用一次；admin /admin/reload/transaction-rules 触发热更新。
	Reload(ctx context.Context) error

	GetMerchantInfo(ctx context.Context, merchantID int64) (*model.MerchantInfo, error)
}

// ruleSnapshot 不可变快照：替换时 atomic.Store 整个指针，读端无锁。
type ruleSnapshot struct {
	rulesByPE map[string][]*model.TransactionRule // "productCode:eventCode" → rules
	accTypes  map[string]*model.AccountTypeInfo   // accountTypeCode → info
	loaded    bool                                // false 表示还没 Reload 过；调用方应触发兜底 DB 查询
}

func emptySnapshot() *ruleSnapshot {
	return &ruleSnapshot{
		rulesByPE: map[string][]*model.TransactionRule{},
		accTypes:  map[string]*model.AccountTypeInfo{},
		loaded:    false,
	}
}

type transactionRuleRepository struct {
	dbManager *database.Manager
	router    *sharding.Router

	snapshot atomic.Pointer[ruleSnapshot]
}

// NewTransactionRuleRepository 创建交易规则仓储
// account_type_info 和 transaction_rule 存放在 account_meta 全局元数据库
// merchant_info 按 merchantID 分库分表
func NewTransactionRuleRepository(dbManager *database.Manager, router *sharding.Router) TransactionRuleRepository {
	r := &transactionRuleRepository{dbManager: dbManager, router: router}
	r.snapshot.Store(emptySnapshot())
	return r
}

// Reload 全表 load → 构造 immutable snapshot → 原子替换。
//
// 失败语义：保留旧快照（至少不让规则查询全部坏掉）；返回错误供 admin 端口告警。
func (r *transactionRuleRepository) Reload(ctx context.Context) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return fmt.Errorf("rule reload: get meta db: %w", err)
	}

	var rules []*model.TransactionRule
	if err := db.WithContext(ctx).Order("id ASC").Find(&rules).Error; err != nil {
		return fmt.Errorf("rule reload: load transaction_rule: %w", err)
	}

	var accTypes []*model.AccountTypeInfo
	if err := db.WithContext(ctx).Order("owner_type ASC").Find(&accTypes).Error; err != nil {
		return fmt.Errorf("rule reload: load account_type_info: %w", err)
	}

	rulesByPE := make(map[string][]*model.TransactionRule, len(rules))
	for _, ru := range rules {
		key := ru.ProductCode + ":" + ru.EventCode
		rulesByPE[key] = append(rulesByPE[key], ru)
	}
	accMap := make(map[string]*model.AccountTypeInfo, len(accTypes))
	for _, a := range accTypes {
		accMap[a.AccountType] = a
	}

	r.snapshot.Store(&ruleSnapshot{
		rulesByPE: rulesByPE,
		accTypes:  accMap,
		loaded:    true,
	})
	return nil
}

// GetRulesByProductAndEvent 从内存快照查规则。snapshot 未 load（启动时 Reload 失败 /
// 还没跑过）时退化为直接查 DB（保证 server 仍可用）。
func (r *transactionRuleRepository) GetRulesByProductAndEvent(ctx context.Context, productCode, eventCode string) ([]*model.TransactionRule, error) {
	key := productCode + ":" + eventCode
	snap := r.snapshot.Load()
	if snap.loaded {
		if rules, ok := snap.rulesByPE[key]; ok {
			return rules, nil
		}
		return nil, fmt.Errorf("no transaction rule found for product_code=%s event_code=%s", productCode, eventCode)
	}

	// fallback: snapshot 没 load，去 DB（启动早期或 Reload 失败时）
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, fmt.Errorf("get meta db failed: %w", err)
	}
	var rules []*model.TransactionRule
	result := db.WithContext(ctx).
		Where("product_code = ? AND event_code = ?", productCode, eventCode).
		Order("id ASC").
		Find(&rules)
	if result.Error != nil {
		return nil, fmt.Errorf("query transaction rules failed: %w", result.Error)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("no transaction rule found for product_code=%s event_code=%s", productCode, eventCode)
	}
	return rules, nil
}

// GetAccountTypeInfo 同上，从内存快照查；未 load 时退化为 DB。
func (r *transactionRuleRepository) GetAccountTypeInfo(ctx context.Context, accountTypeCode string) (*model.AccountTypeInfo, error) {
	snap := r.snapshot.Load()
	if snap.loaded {
		if info, ok := snap.accTypes[accountTypeCode]; ok {
			return info, nil
		}
		return nil, fmt.Errorf("account type info not found: %s", accountTypeCode)
	}

	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, fmt.Errorf("get meta db failed: %w", err)
	}
	var info model.AccountTypeInfo
	result := db.WithContext(ctx).Where("account_type = ?", accountTypeCode).First(&info)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("account type info not found: %s", accountTypeCode)
		}
		return nil, fmt.Errorf("query account type info failed: %w", result.Error)
	}
	return &info, nil
}

// ListRulesByProduct 列指定 product_code 下所有规则.
//
// 优先用内存快照 (启动时已 Reload); 未 load → fall back 到 DB.
// productCode 空 → 返回全部规则 (admin 总览用).
func (r *transactionRuleRepository) ListRulesByProduct(ctx context.Context, productCode string) ([]*model.TransactionRule, error) {
	snap := r.snapshot.Load()
	if snap.loaded {
		out := []*model.TransactionRule{}
		for _, rules := range snap.rulesByPE {
			for _, ru := range rules {
				if productCode == "" || ru.ProductCode == productCode {
					out = append(out, ru)
				}
			}
		}
		return out, nil
	}
	// fallback: snapshot 没 load,去 DB
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, fmt.Errorf("list rules: get meta db: %w", err)
	}
	var rules []*model.TransactionRule
	q := db.WithContext(ctx).Order("id ASC")
	if productCode != "" {
		q = q.Where("product_code = ?", productCode)
	}
	if err := q.Find(&rules).Error; err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	return rules, nil
}

// ListAccountTypes 全表扫 account_type_info（条目少、admin-web 偶尔拉取，不走缓存）。
func (r *transactionRuleRepository) ListAccountTypes(ctx context.Context) ([]*model.AccountTypeInfo, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, fmt.Errorf("list account_type_info: get meta db: %w", err)
	}
	var rows []*model.AccountTypeInfo
	if err := db.WithContext(ctx).Order("owner_type ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list account_type_info: %w", err)
	}
	return rows, nil
}

// GetMerchantInfo 查询商户信息（按 merchantID 分库分表，不缓存——商户数据可能变更）
func (r *transactionRuleRepository) GetMerchantInfo(ctx context.Context, merchantID int64) (*model.MerchantInfo, error) {
	dbIndex, tableIndex := r.router.RouteByID(merchantID)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("get db[%d] failed: %w", dbIndex, err)
	}
	tableName := r.router.GetTableName("merchant_info", tableIndex)

	var info model.MerchantInfo
	result := db.WithContext(ctx).Table(tableName).Where("merchant_id = ?", merchantID).First(&info)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("merchant not found: %d", merchantID)
		}
		return nil, fmt.Errorf("query merchant info failed: %w", result.Error)
	}

	return &info, nil
}
