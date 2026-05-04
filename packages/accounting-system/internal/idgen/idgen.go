// Package idgen 提供号段模式（Leaf Segment）ID 生成器，直接集成到 accounting-system。
//
// 号段模式特点：
//   - ID 严格单调递增，基于 MySQL max_id 推进，不依赖时钟
//   - 双 Buffer 设计：current 号段消耗到 loadFactor (90%) 时异步预加载 next 号段，
//     切换瞬间无停顿，正常路径完全无 DB IO
//   - 服务重启、时间漂移均不影响 ID 单调性
//   - leaf_alloc 表存于 account_meta（元数据库，独立于分库分表）
//
// 业务标签（BizTag）：
//   - BizTagVoucher     → accounting_voucher_XX 的 voucher_no 后缀
//   - BizTagTransaction → account_transaction_XX 的 transaction_id 后缀
package idgen

import (
	"context"
	"fmt"

	"github.com/accounting-system/internal/infrastructure/database"
	"go.uber.org/zap"
)

// 业务标签常量
const (
	BizTagVoucher     = "accounting.voucher"      // 凭证号（accounting_voucher 表）
	BizTagTransaction = "accounting.transaction"  // 流水号（account_transaction 表）
	BizTagAsyncTask   = "accounting.async_task"   // 异步任务ID（async_task 表）
)

// defaultInitMaxID 业务标签初始 max_id（首次注册时使用）
const defaultInitMaxID = 1_000_000

// defaultStep 高频业务号段步长：每次从 DB 分配 100000 个 ID，正常流量下约每分钟一次 DB 访问
const defaultStep = 100_000

// NewIDGeneratorFromManager 从 accounting-system 的 DB Manager 创建号段 ID 生成器。
// leaf_alloc 表使用 account_meta 元数据库（独立于分库分表，不随分片扩容而复制）。
// 启动时自动注册 accounting.voucher / accounting.transaction / accounting.async_task 三个 biz_tag（幂等）。
func NewIDGeneratorFromManager(mgr *database.Manager, logger *zap.Logger) (IDGenerator, error) {
	db, err := mgr.GetMetaDB()
	if err != nil {
		return nil, fmt.Errorf("idgen: get meta DB: %w", err)
	}

	repo := newSegmentRepository(db)
	gen := newSegmentIDGenerator(repo, 0.9, logger)

	// 自动注册 biz_tag（若已存在则幂等跳过）
	ctx := context.Background()
	for _, tag := range []struct {
		name string
		desc string
	}{
		{BizTagVoucher, "账务系统凭证号（accounting_voucher）"},
		{BizTagTransaction, "账务系统流水号（account_transaction）"},
		{BizTagAsyncTask, "异步任务ID（async_task）"},
	} {
		if err := gen.Register(ctx, tag.name, defaultInitMaxID, defaultStep, tag.desc); err != nil {
			return nil, fmt.Errorf("idgen: register biz_tag %q: %w", tag.name, err)
		}
	}

	// 预热：加载所有 biz_tag 的首个号段到内存，消除首次请求的 DB 延迟
	if err := gen.Preload(ctx); err != nil {
		// 预热失败不阻断启动（首次请求会同步加载），仅记录警告
		logger.Warn("idgen: preload failed, IDs will be loaded on first request", zap.Error(err))
	}

	return gen, nil
}
