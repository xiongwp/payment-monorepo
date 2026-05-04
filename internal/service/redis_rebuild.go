// Package service — Redis hot-account rebuild from MySQL.
//
// 用途：当 Redis 数据丢失 / 损坏 / 怀疑被脏写时，从 MySQL 这唯一权威源
// 重建 Redis 上的热点账户余额。只重建 hot_account_config 中 enabled=1
// 的账户；非热点账户 Redis 本来就不存权威态，无需处理。
//
// 选项：
//   - asOf == zero       → 用 account 表的当前 balance（含所有已 settle 的 outbox）
//   - asOf == 时间戳      → 在 account_transaction 流水里回放，定位 transaction_time
//                          ≤ asOf 的最后一笔，取其 balance_after 作为目标余额。
//                          用于"怀疑最近 N 分钟的写入有问题，回滚到那之前"的场景。
//   - dryRun             → 只算出目标余额，不写 Redis；返回 diff 报告。
//   - accountNos 非空     → 仅重建指定账户（必须仍然在 hot_account_config）。
package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	commonutil "github.com/accounting-system/internal/common"
	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/cache"
)

// RebuildOptions 重建配置。
type RebuildOptions struct {
	AsOf        time.Time // zero 表示 "now"，用 account 表当前余额
	AccountNos  []string  // 空表示从 hot_account_config 取全量启用账户
	DryRun      bool      // true 时只计算 diff，不写 Redis
	Concurrency int       // 单分片并发上限，默认 16
}

// RebuildEntry 单账户的重建结果。
type RebuildEntry struct {
	AccountNo       string `json:"account_no"`
	BalanceBefore   string `json:"balance_before"`   // Redis 重建前余额（"" 表示 cache miss）
	BalanceAfter    string `json:"balance_after"`    // 目标余额（重建后）
	Source          string `json:"source"`           // "account" | "transaction_journal"
	JournalCutoff   string `json:"journal_cutoff,omitempty"`
	Skipped         bool   `json:"skipped"`          // dryRun 或账户不存在等
	Reason          string `json:"reason,omitempty"` // skip 原因 / 错误
}

// RebuildReport 一次重建的汇总。
type RebuildReport struct {
	AsOf      time.Time      `json:"as_of"`
	DryRun    bool           `json:"dry_run"`
	Total     int            `json:"total"`
	Updated   int            `json:"updated"`
	Skipped   int            `json:"skipped"`
	Failed    int            `json:"failed"`
	StartedAt time.Time      `json:"started_at"`
	Duration  string         `json:"duration"`
	Entries   []RebuildEntry `json:"entries,omitempty"`
}

// RebuildHotAccounts 主入口。
//
// 算法：
//   1. 拉 hot_account_config（或用 opts.AccountNos 过滤后的子集）
//   2. 每个账户路由到自己的 shard，并发处理（per-shard worker pool）
//   3. 计算目标余额（asOf 决定走 account 表还是流水回放）
//   4. dryRun 时跳过 Redis 写；否则 HSET balance:<account_no>
//
// 注意：本函数 **不** 删 idem key。如果因为时间戳回滚使得某些已 apply 的
// voucher 对应的 idem key 留在 Redis 里，再次重放（recovery）时会被
// `already_applied` 分支吞掉 —— 副作用是这些 voucher 此后不会重 apply 到
// Redis（直到 idem TTL 过期），所以"回滚到 asOf 之前"的语义对老 voucher
// 是只读不重做。如果需要彻底回滚，应在重建后手动 DEL idem:transfer:*
// 或者在 cache 层加一个新接口（FlushIdemKeysAfter(asOf)）。
func (s *accountingService) RebuildHotAccounts(ctx context.Context, opts RebuildOptions) (*RebuildReport, error) {
	if s.balanceCache == nil {
		return nil, fmt.Errorf("rebuild: hot path not enabled (balanceCache == nil)")
	}
	if s.hotAccountRepo == nil {
		return nil, fmt.Errorf("rebuild: hot account repo not wired")
	}
	if s.router == nil || s.dbManager == nil {
		return nil, fmt.Errorf("rebuild: router/dbManager not wired")
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 16
	}

	report := &RebuildReport{
		AsOf:      opts.AsOf,
		DryRun:    opts.DryRun,
		StartedAt: time.Now(),
	}

	// ── 1. 解析待处理账户集合 ────────────────────────────────────────
	allowed, err := s.hotAccountRepo.LoadEnabledAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("rebuild: load enabled hot accounts: %w", err)
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		allowedSet[a] = struct{}{}
	}
	var targets []string
	if len(opts.AccountNos) > 0 {
		// 只处理 caller 指定且仍在白名单的账户
		for _, a := range opts.AccountNos {
			if _, ok := allowedSet[a]; ok {
				targets = append(targets, a)
			} else {
				report.Entries = append(report.Entries, RebuildEntry{
					AccountNo: a,
					Skipped:   true,
					Reason:    "not in hot_account_config (or disabled)",
				})
				report.Skipped++
			}
		}
	} else {
		targets = allowed
	}
	report.Total = len(targets) + report.Skipped

	// ── 2. 并发处理 ─────────────────────────────────────────────────
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, accountNo := range targets {
		accountNo := accountNo
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			entry := s.rebuildOne(ctx, accountNo, opts)
			mu.Lock()
			report.Entries = append(report.Entries, entry)
			switch {
			case entry.Reason != "" && !entry.Skipped:
				report.Failed++
			case entry.Skipped:
				report.Skipped++
			default:
				report.Updated++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	report.Duration = time.Since(report.StartedAt).String()
	s.logger.Info("rebuild hot accounts completed",
		zap.Bool("dry_run", opts.DryRun),
		zap.Time("as_of", opts.AsOf),
		zap.Int("total", report.Total),
		zap.Int("updated", report.Updated),
		zap.Int("skipped", report.Skipped),
		zap.Int("failed", report.Failed),
		zap.String("duration", report.Duration),
	)
	return report, nil
}

func (s *accountingService) rebuildOne(ctx context.Context, accountNo string, opts RebuildOptions) RebuildEntry {
	entry := RebuildEntry{AccountNo: accountNo}

	dbIdx, tableIdx := s.router.RouteByAccountNo(accountNo)
	db, err := s.dbManager.GetDB(dbIdx)
	if err != nil {
		entry.Reason = fmt.Sprintf("get db[%d]: %v", dbIdx, err)
		return entry
	}

	// 拿 account 元信息（type / category / status / version）。
	accountTable := s.router.GetTableName("account", tableIdx)
	var acc model.Account
	if err := db.WithContext(ctx).Table(accountTable).
		Where("account_no = ?", accountNo).
		Take(&acc).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			entry.Skipped = true
			entry.Reason = "account row not found in MySQL"
			return entry
		}
		entry.Reason = fmt.Sprintf("load account: %v", err)
		return entry
	}

	// 算目标余额
	balance, available, frozen := acc.Balance, acc.AvailableBalance, acc.FrozenBalance
	source := "account"
	if !opts.AsOf.IsZero() {
		// 回放模式：从流水拿 asOf 时刻的最后一笔 balance_after
		txTable := s.router.GetTableName("account_transaction", tableIdx)
		var lastTx model.AccountTransaction
		err := db.WithContext(ctx).Table(txTable).
			Where("account_no = ? AND transaction_time <= ?", accountNo, opts.AsOf).
			Order("transaction_time DESC").
			Limit(1).
			Take(&lastTx).Error
		switch {
		case err == nil:
			balance = lastTx.BalanceAfter
			// available / frozen 流水里没单独存，按"无未结冻结增量"处理：
			//   available = balance - frozen_at_asOf
			// 简化：保留 account.frozen_balance（冻结/解冻数量在流水可重算，但
			// 表里没有冻结状态字段 → 这里明示给个最佳近似，调用方可据 source
			// 决定是否信任）
			available = balance - acc.FrozenBalance
			frozen = acc.FrozenBalance
			source = "transaction_journal"
		case err == gorm.ErrRecordNotFound:
			// asOf 早于该账户首笔流水：要么账户还没活动，要么所有流水都在 asOf 之后。
			// 用 0 作 asOf 余额（账户的"出生"状态）。
			balance, available, frozen = 0, 0, 0
			source = "transaction_journal"
		default:
			entry.Reason = fmt.Sprintf("load tx: %v", err)
			return entry
		}
		entry.JournalCutoff = opts.AsOf.Format(time.RFC3339)
	}
	entry.Source = source
	entry.BalanceAfter = strconv.FormatInt(balance, 10)

	// 读 Redis 前态做 diff
	if before, gerr := s.balanceCache.GetBalance(ctx, accountNo); gerr == nil && before != nil {
		entry.BalanceBefore = before.Balance
	} else {
		entry.BalanceBefore = "" // miss
	}

	if opts.DryRun {
		entry.Skipped = true
		entry.Reason = "dry-run"
		return entry
	}

	cat := 1 // ASSET / EXPENSE → 1
	if !commonutil.IsAccountCanNegative(acc.AccountType) {
		// USER / MERCHANT / MERCHANT_PENDING_SETTLE → 不可负 → LIAB
		cat = 2
	} else if acc.AccountCategory == model.AccountCategoryLiability ||
		acc.AccountCategory == model.AccountCategoryEquity ||
		acc.AccountCategory == model.AccountCategoryRevenue {
		cat = 2
	}

	if err := s.balanceCache.WarmAccount(ctx, cache.BalanceInfo{
		AccountNo: accountNo,
		Balance:   strconv.FormatInt(balance, 10),
		Available: strconv.FormatInt(available, 10),
		Frozen:    strconv.FormatInt(frozen, 10),
		Version:   acc.Version,
		Category:  cat,
		Status:    int(acc.Status),
	}); err != nil {
		entry.Reason = fmt.Sprintf("warm redis: %v", err)
		return entry
	}
	return entry
}

// 防止 import 报 "imported and not used" 的占位（strings 在文档示例里可能用到）。
var _ = strings.TrimSpace
