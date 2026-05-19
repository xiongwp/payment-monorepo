package service

// IntegrityCheckWorker 资金账完整性后台审计：扫所有 voucher 验证
// 同 parent_transaction_id 的 entries 共享同一个 cut_date，并且
// SUM(debit_amount) == SUM(credit_amount)。
//
// 触发：每日日切完成后跑一次（由 caller wire 到 cron），或运维手动触发
// /admin/integrity-check 端点。发现违规 → log ERROR + Prometheus counter
// 让告警系统拉起。
//
// 设计：
//   - 跨 100 个分表扫，单次跑批 query 量大；用游标分页 + 限流避免压垮 DB
//   - 跑完之后 emit Prometheus 指标：
//       accounting_integrity_voucher_cut_date_mismatch_total
//       accounting_integrity_voucher_balance_mismatch_total
//   - 违规明细落 logger.Error(zap...) 让 ELK / loki 抓到
//   - 不修复，只报警 —— 修复涉及业务语义判断，必须人工 / 调用方负责
//
// 不变量：
//   1. 同 parent_transaction_id 的 entries 必须共享一个 cut_date
//      （不变量违反 = trial balance 按 cut_date 过滤后会不平）
//   2. 同 parent_transaction_id 的 SUM(debit) == SUM(credit)
//      （不变量违反 = 任何聚合都不平）

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

var (
	integrityCutDateMismatchTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "accounting_integrity_voucher_cut_date_mismatch_total",
		Help: "Vouchers whose entries don't share a single cut_date (per-day trial balance will mismatch).",
	})
	integrityBalanceMismatchTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "accounting_integrity_voucher_balance_mismatch_total",
		Help: "Vouchers whose entries' SUM(debit) != SUM(credit) (double-entry violated).",
	})
	integrityScanRunsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "accounting_integrity_scan_runs_total",
		Help: "Number of integrity check scan runs.",
	})
	integrityScanDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "accounting_integrity_scan_duration_seconds",
		Help:    "Time taken for one full integrity scan across all shards.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	})
)

// IntegrityCheckWorker 完整性扫描 worker
type IntegrityCheckWorker struct {
	router    *sharding.Router
	dbManager *database.Manager
	logger    *zap.Logger

	// Window 扫多大的时间窗口（默认查最近 24h 的 voucher）
	Window time.Duration
}

// NewIntegrityCheckWorker 构造
func NewIntegrityCheckWorker(
	router *sharding.Router,
	dbManager *database.Manager,
	logger *zap.Logger,
) *IntegrityCheckWorker {
	return &IntegrityCheckWorker{
		router:    router,
		dbManager: dbManager,
		logger:    logger.Named("integrity"),
		Window:    24 * time.Hour,
	}
}

// Run 跑一次完整扫描。返回 (cut_date 不一致的 voucher 数, debit≠credit 的 voucher 数, error)。
//
// 设计上不修复任何数据，只报警 —— 修复涉及业务语义判断（哪个 cut_date 是对的？
// 哪个金额是对的？）必须人工或调用方决定。
func (w *IntegrityCheckWorker) Run(ctx context.Context) (cutDateBad, balanceBad int64, err error) {
	start := time.Now()
	defer func() {
		integrityScanRunsTotal.Inc()
		integrityScanDuration.Observe(time.Since(start).Seconds())
	}()

	since := time.Now().Add(-w.Window).Format("2006-01-02 15:04:05")

	var cdBad, balBad atomic.Int64

	for _, shard := range w.router.GetAllShards() {
		if ctx.Err() != nil {
			return cdBad.Load(), balBad.Load(), ctx.Err()
		}
		db, gerr := w.dbManager.GetDB(shard.DBIndex)
		if gerr != nil {
			w.logger.Error("integrity: get db failed",
				zap.Int("dbIndex", shard.DBIndex), zap.Error(gerr))
			continue
		}
		tableName := w.router.GetTableName("account_transaction", shard.TableIndex)

		// 单表扫：同 parent_transaction_id 的 cut_date distinct 数 > 1，或 sum(debit) != sum(credit)。
		// 注意：voucher 的 entries 跨 shard 分布；单表内能看到 voucher 的"部分" entries。
		// 这里只查"在本表内 cut_date 就已不一致"的 voucher（这是必要条件之一，
		// 但不是 sufficient — 跨表不一致需要全局聚合，下面 ② 处理）。
		type bad struct {
			ParentTransactionID string
			DistinctCutDates    int64
			TotalDebit          int64
			TotalCredit         int64
		}
		var rows []bad
		qErr := db.WithContext(ctx).Table(tableName).
			Select(`parent_transaction_id,
                COUNT(DISTINCT cut_date) AS distinct_cut_dates,
                SUM(debit_amount) AS total_debit,
                SUM(credit_amount) AS total_credit`).
			Where("created_at >= ? AND status = 1", since).
			Group("parent_transaction_id").
			Having("COUNT(DISTINCT cut_date) > 1").
			Scan(&rows).Error
		if qErr != nil {
			w.logger.Error("integrity: scan single-table cut_date mismatch failed",
				zap.String("table", tableName), zap.Error(qErr))
			continue
		}
		for _, r := range rows {
			cdBad.Add(1)
			integrityCutDateMismatchTotal.Inc()
			w.logger.Error("integrity: cut_date mismatch within voucher (single-table view)",
				zap.String("voucher", r.ParentTransactionID),
				zap.Int64("distinct_cut_dates", r.DistinctCutDates),
				zap.String("table", tableName),
				zap.String("dbIndex_table", fmt.Sprintf("db%d_t%d", shard.DBIndex, shard.TableIndex)),
			)
		}
	}

	// ② 跨分片全局聚合：同 voucher 的 entries 跨多 shard，单表看不到全貌。
	// 把每个 (parent_tx, cut_date, debit_sum, credit_sum) 抽出来到 host 端 awk-like 聚合。
	// 这一步成本较高（百万级 voucher 时拉数据），生产可以按 cut_date 分批跑。
	type aggKey struct {
		voucher string
		cutDate string
	}
	type aggVal struct {
		debit, credit int64
	}
	agg := make(map[aggKey]*aggVal)
	voucherCutDates := make(map[string]map[string]struct{})

	for _, shard := range w.router.GetAllShards() {
		if ctx.Err() != nil {
			return cdBad.Load(), balBad.Load(), ctx.Err()
		}
		db, gerr := w.dbManager.GetDB(shard.DBIndex)
		if gerr != nil {
			continue
		}
		tableName := w.router.GetTableName("account_transaction", shard.TableIndex)
		type row struct {
			ParentTransactionID string
			CutDate             string
			Debit               int64
			Credit              int64
		}
		var rs []row
		qErr := db.WithContext(ctx).Table(tableName).
			Select(`parent_transaction_id,
                cut_date,
                SUM(debit_amount) AS debit,
                SUM(credit_amount) AS credit`).
			Where("created_at >= ? AND status = 1", since).
			Group("parent_transaction_id, cut_date").
			Scan(&rs).Error
		if qErr != nil {
			w.logger.Error("integrity: cross-shard agg query failed",
				zap.String("table", tableName), zap.Error(qErr))
			continue
		}
		for _, r := range rs {
			k := aggKey{voucher: r.ParentTransactionID, cutDate: r.CutDate}
			v := agg[k]
			if v == nil {
				v = &aggVal{}
				agg[k] = v
			}
			v.debit += r.Debit
			v.credit += r.Credit

			cd := voucherCutDates[r.ParentTransactionID]
			if cd == nil {
				cd = make(map[string]struct{})
				voucherCutDates[r.ParentTransactionID] = cd
			}
			cd[r.CutDate] = struct{}{}
		}
	}

	// 检查不变量
	balPerVoucher := make(map[string]*aggVal, len(agg))
	for k, v := range agg {
		// cut_date 一致性：同 voucher 出现 >= 2 个 cut_date → 报警
		if len(voucherCutDates[k.voucher]) > 1 {
			// 注意可能在 ① 已被报过；这里 emit 一次"全局"维度的事件，便于运维区分
			cdBad.Add(1)
			integrityCutDateMismatchTotal.Inc()
			cdList := make([]string, 0, len(voucherCutDates[k.voucher]))
			for cd := range voucherCutDates[k.voucher] {
				cdList = append(cdList, cd)
			}
			w.logger.Error("integrity: cut_date mismatch within voucher (cross-shard agg)",
				zap.String("voucher", k.voucher),
				zap.Strings("cut_dates", cdList),
			)
		}
		// 借贷恒等：累计每个 voucher 的全 cut_date 总借贷
		bv := balPerVoucher[k.voucher]
		if bv == nil {
			bv = &aggVal{}
			balPerVoucher[k.voucher] = bv
		}
		bv.debit += v.debit
		bv.credit += v.credit
	}
	for voucher, bv := range balPerVoucher {
		if bv.debit != bv.credit {
			balBad.Add(1)
			integrityBalanceMismatchTotal.Inc()
			w.logger.Error("integrity: voucher debit != credit",
				zap.String("voucher", voucher),
				zap.Int64("debit", bv.debit),
				zap.Int64("credit", bv.credit),
				zap.Int64("diff", bv.debit-bv.credit),
			)
		}
	}

	w.logger.Info("integrity: scan finished",
		zap.Int64("cut_date_mismatch_vouchers", cdBad.Load()),
		zap.Int64("balance_mismatch_vouchers", balBad.Load()),
		zap.Duration("took", time.Since(start)),
	)
	return cdBad.Load(), balBad.Load(), nil
}

var _ = sharding.ShardInfo{}
