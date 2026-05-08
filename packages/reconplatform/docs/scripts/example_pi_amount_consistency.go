// Example：PI 三表金额一致性对账。
//
// 这是一个示例脚本，展示运营在 admin web 编辑器里能写什么。实际部署时这段
// 源码存在 Redis (recon:script:<id>)，由 script.Loader 通过 Yaegi 解释器加载。
//
// 业务场景：用户支付一笔 100 元，三个表必须金额一致：
//   - order-core.payment_intents.amount
//   - accounting-system.account_transaction.amount  (related_pi_id=PI)
//   - payment-channel.card_charges.amount           (pi_id=PI)
//
// 三方任何一方缺失或金额不一致 → 资损嫌疑，写 diff 给运营人工核查。
//
// 编辑器 autocomplete 会从 /api/v1/meta/* 拉表 schema，输入 "events.Find("
// 时弹出可选的 (svc, table) 组合；输入 ".Int(" 时弹出可选列名。
//
// 触发方式：
//   - manual: admin 点 "Run Now"
//   - cron:   "*/15 * * * *"  (每 15 分钟扫一次最近 24h 的 PI)
//   - stream: ["accounting-system:account_transaction", "payment-channel:card_charges"]
//             accounting / channel 一有事件落库立即触发（早期发现差异）
//
// 性能：扫 1h 内 ~5K PI 大概 5K × 3 次 GetByIndex = 15K Redis SMEMBERS+MGET，
// 在本地 Redis 上 ~2 秒；admin web "Run Now" 同步等结果体验 OK。
//
// ----------------------------------------------------------------------
package main

import (
	"fmt"
	"time"

	"recon" // Yaegi 注入的 recon API（实际 = script 包）
)

// Check 是脚本入口；签名固定 func(*recon.Context) (*recon.Result, error)。
func Check(ctx *recon.Context) (*recon.Result, error) {
	r := &recon.Result{} // engine 会填 ScriptID / RunID 等元信息

	// 扫最近 24 小时所有 pi_id（Redis SCAN 限 5000 条 / 次）。
	// 实际想覆盖更长时段：用 cron 高频跑（每 5min 扫 1h），分摊到多次运行。
	since, _ := recon.LastNHours(ctx.Now, 24)
	piIDs := ctx.ScanIndex("pi_id", "", 5000)

	if len(piIDs) == 0 {
		ctx.Logger.Info("no pi_id events in window", "since", since)
		return r, nil
	}

	for _, pi := range piIDs {
		events := ctx.GetByIndex("pi_id", pi)
		if len(events) == 0 {
			continue // 索引 SET 还没 GC，但主存已过期 → 跳过
		}

		order := events.Find("order-core", "payment_intents")
		txn := events.Find("accounting-system", "account_transaction")
		chg := events.Find("payment-channel", "card_charges")

		// case 1: 缺腿（任意一方没找到）
		missing := []string{}
		if order.IsEmpty() {
			missing = append(missing, "order-core/payment_intents")
		}
		if txn.IsEmpty() {
			missing = append(missing, "accounting-system/account_transaction")
		}
		if chg.IsEmpty() {
			missing = append(missing, "payment-channel/card_charges")
		}
		if len(missing) > 0 && len(missing) < 3 {
			// 全缺意味着事件还没传过来（或者本来就不是支付流），不报；
			// 部分缺才是真问题。
			ctx.AddDiff("missing_leg", pi, fmt.Sprintf("missing: %v", missing))
			continue
		}
		if len(missing) == 3 {
			continue
		}

		// case 2: 金额不一致
		want := order.Int("amount")
		gotTxn := txn.Int("amount")
		gotChg := chg.Int("amount")
		if want != gotTxn || want != gotChg {
			ctx.AddCompare("amount_mismatch", pi, want, map[string]int64{
				"order-core/payment_intents":             want,
				"accounting-system/account_transaction":  gotTxn,
				"payment-channel/card_charges":           gotChg,
			})
			continue
		}

		// case 3: PI succeeded 但 channel 还在 pending（可能 webhook 漏了）
		if order.Str("status") == "succeeded" && chg.Str("status") != "succeeded" {
			ctx.AddDiff("pi_succeeded_but_charge_not", pi, map[string]string{
				"pi_status":     order.Str("status"),
				"charge_status": chg.Str("status"),
				"updated_at":    chg.Str("updated_at"),
			})
		}

		// case 4: PI 创建超过 1h 但 ledger 还没落（accounting 慢）
		if !order.IsEmpty() && txn.IsEmpty() {
			createdAt, _ := time.Parse(time.RFC3339, order.Str("created_at"))
			if !createdAt.IsZero() && time.Since(createdAt) > time.Hour {
				ctx.AddDiff("ledger_lag", pi, fmt.Sprintf("PI %s created %s ago, no txn", pi, time.Since(createdAt)))
			}
		}
	}

	ctx.Logger.Info("pi consistency check done", "scanned", len(piIDs), "diffs", len(r.Diffs))
	return r, nil
}
