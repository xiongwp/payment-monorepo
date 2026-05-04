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
	"time"

	"github.com/accounting-system/internal/infrastructure/database"
	"go.uber.org/zap"
)

// 业务标签常量。biz_tag 在 leaf_alloc 表里独立分号段，互不干扰。
//
// 命名约定：`accounting.<entity>` —— 跟其他系统隔开（order-core 用 `order.*`,
// user-merchant 用 `usermerchant.*` 等）防止 leaf_alloc 跨系统冲突。
const (
	BizTagAccount     = "accounting.account"      // 账户号 seq（account_no encode 用，每 (currency, type, gtbl, biz) 组合一段）
	BizTagVoucher     = "accounting.voucher"      // 凭证号（accounting_voucher 表）
	BizTagTransaction = "accounting.transaction"  // 流水号（account_transaction 表）
	BizTagAsyncTask   = "accounting.async_task"   // 异步任务ID（async_task 表）
	BizTagFreezeTx    = "accounting.freeze_tx"    // 冻结流水号（freeze_compensate_outbox 关联）
	BizTagRequestID   = "accounting.request_id"   // facade 层请求 ID（去重 key 兜底）
)

// defaultInitMaxID 业务标签初始 max_id（首次注册时使用）
const defaultInitMaxID = 1_000_000

// defaultStep 高频业务号段步长：每次从 DB 分配 100000 个 ID，正常流量下约每分钟一次 DB 访问
const defaultStep = 100_000

// 自适应步长（P1-12 横向扩展瓶颈缓解）：
//   - 观测：同一 bizTag 两次 loadSegment 间隔 < adaptiveStepFastThreshold → step 倍增
//   - 上限 maxStep 防止单次分配过大造成 leaf_alloc 表跳号过宽（重启 / 故障损耗）
//   - 上限到 16× default = 1.6M / 单次分配，足以撑住 1k-10k QPS bizTag
//
// 不做向下回退（流量低时 step 大也无害；多分配的号段重启就丢，与小 step 同样情况）。
const (
	adaptiveStepFastThreshold = 30 * time.Second // 30s 内连续两次 reload → 触发倍增
	adaptiveStepMaxMultiplier = 16               // 上限：default × 16
)

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
		{BizTagAccount, "账户号 seq（account_no encode 用）"},
		{BizTagVoucher, "账务系统凭证号（accounting_voucher）"},
		{BizTagTransaction, "账务系统流水号（account_transaction）"},
		{BizTagAsyncTask, "异步任务ID（async_task）"},
		{BizTagFreezeTx, "冻结流水号（freeze_compensate_outbox）"},
		{BizTagRequestID, "facade 请求 ID（去重兜底）"},
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
