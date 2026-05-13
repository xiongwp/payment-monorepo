// trial-balance — 日切 trial balance 检查工具.
//
// 跑法 (K8s CronJob 每日 23:55):
//   ./trial-balance --date 2026-05-13 --currency USD
//
// 输出:
//   非零退出码 → 不平 → cron 失败 → alertmanager P0 page (fund-safety oncall)
//
// 实现:
//   1. 拉昨日全量 fee_event / refund_event / chargeback_event
//   2. 计算 net = sum(charge) - sum(refund) - sum(chargeback)
//   3. 跟 settlement.net_payout 表加总比对
//   4. diff != 0 → 写 reconplatform diff 表 (P0 规则) + 退非零
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	var (
		dsn      = flag.String("dsn", os.Getenv("METADB_DSN"), "MySQL DSN")
		date     = flag.String("date", "", "yyyy-mm-dd (default: yesterday UTC)")
		currency = flag.String("currency", "USD", "currency")
		tolerance = flag.Int64("tolerance", 0, "abs diff threshold in cents (0 = strict)")
	)
	flag.Parse()
	if *dsn == "" {
		log.Fatal("--dsn or METADB_DSN required")
	}
	if *date == "" {
		*date = time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	}

	db, err := sql.Open("mysql", *dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(30 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	diff, err := check(ctx, db, *date, *currency, *tolerance)
	if err != nil {
		log.Fatalf("trial balance check failed: %v", err)
	}
	if diff.Failed {
		// 写到 reconplatform diff 队列 (P0 规则)
		if err := emitReconDiff(ctx, db, diff); err != nil {
			log.Printf("WARNING: emit recon diff failed (alert is best-effort): %v", err)
		}
		fmt.Fprintf(os.Stderr,
			"TRIAL_BALANCE_FAIL: date=%s currency=%s ledger_net=%d settle_net=%d diff=%d tolerance=%d\n",
			*date, *currency, diff.LedgerNet, diff.SettleNet, diff.DiffCents, *tolerance)
		os.Exit(2) // 非零退出 → CronJob 失败 → PrometheusRule 触发
	}
	fmt.Printf("OK date=%s currency=%s net=%d\n", *date, *currency, diff.LedgerNet)
}

// diffResult 检查输出.
type diffResult struct {
	Date       string
	Currency   string
	LedgerNet  int64
	SettleNet  int64
	DiffCents  int64
	Failed     bool
}

// check 跑核心 SQL.
//
// SQL 设计:
//   - 用 sum + group by 一条 SQL 跑完日维度净值
//   - settlement 表是 net 已经算好,直接 sum
func check(ctx context.Context, db *sql.DB, date, currency string, tolerance int64) (*diffResult, error) {
	out := &diffResult{Date: date, Currency: currency}

	// 1) 账本净值: charge - refund - chargeback (按 occurred_at 当日)
	err := db.QueryRowContext(ctx, `
SELECT
  COALESCE(SUM(CASE WHEN event_type = 'charge'                     THEN amount_minor ELSE 0 END), 0) -
  COALESCE(SUM(CASE WHEN event_type IN ('refund','chargeback')     THEN amount_minor ELSE 0 END), 0)
FROM fee_event
WHERE currency = ? AND DATE(occurred_at) = ?
`, currency, date).Scan(&out.LedgerNet)
	if err != nil {
		return nil, fmt.Errorf("ledger net: %w", err)
	}

	// 2) 清算净值: 同期 settlement 记录的总应付
	err = db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(net_payout_minor), 0)
FROM settlement
WHERE currency = ? AND settle_date = ? AND status = 'COMPLETED'
`, currency, date).Scan(&out.SettleNet)
	if err != nil {
		return nil, fmt.Errorf("settle net: %w", err)
	}

	out.DiffCents = out.LedgerNet - out.SettleNet
	if absInt64(out.DiffCents) > tolerance {
		out.Failed = true
	}
	return out, nil
}

// emitReconDiff 把不平结果落到 reconplatform 的 diff 表.
// reconplatform 自带 P0 规则,会立刻 page。
func emitReconDiff(ctx context.Context, db *sql.DB, d *diffResult) error {
	const q = `
INSERT INTO recon_diff
  (catalog_rule, severity, occurred_at, payload_json)
VALUES
  ('trial_balance', 'P0', NOW(), JSON_OBJECT(
    'date', ?, 'currency', ?,
    'ledger_net', ?, 'settle_net', ?,
    'diff_cents', ?
  ))`
	_, err := db.ExecContext(ctx, q, d.Date, d.Currency, d.LedgerNet, d.SettleNet, d.DiffCents)
	return err
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
