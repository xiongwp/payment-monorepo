# channel_in_accounting — 三方对账 (2/8): channel → accounting 存在性.
# meta-version: 1
#
# 目的:
#   payment-channel 落的 succeeded acquirer_tx,
#   是否在 accounting-system 有对应的 ledger_entry?
#
# 触发场景:
#   - 渠道 commit 但 accounting 的 outbox event 没投递
#   - accounting consumer 卡死 / 跨库事务半提交
#   - 资金真扣了但账目对不上 -> 资损前兆
#
# 严重度: critical (资金已动账目不一致 = 直接资损)

def check(ctx):
    diffs = []
    txs = ctx.scan("payment-channel", "acquirer_tx")

    for tx in txs:
        if tx.str("status") != "succeeded":
            continue
        pi_id = tx.str("pi_id")
        if pi_id == "":
            pi_id = tx.str("payment_intent_id")
        if pi_id == "":
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        ledger = related.find("accounting-system", "ledger_entry")

        if not ledger:
            diffs.append({
                "type": "channel_no_accounting_leg",
                "key": pi_id,
                "detail": {
                    "tx_id":    tx.str("id") or tx.pk,
                    "amount":   tx.int("amount"),
                    "currency": tx.str("currency"),
                    "channel_succeeded_at": tx.str("succeeded_at") or tx.str("updated_at"),
                    "verdict":  "orphan",
                    "severity": "critical",
                    "hint": "渠道已扣款但 accounting 未记账 — outbox 未投递或 consumer 卡住",
                },
            })
    return diffs
