# channel_in_accounting — 三方对账之 (2):
#
# payment-channel 落账的 acquirer_tx (succeeded) 是否在 accounting-system 有
# 对应的 ledger_entry?
#
# 触发场景:
#   - channel 成功了但 outbox event 没投递 / accounting consumer 卡死
#   - 跨库事务半提交 (channel commit、accounting rollback)
#   - 资金已经从用户扣出但平台账目对不上 — 资损前兆
#
# 严重度: critical (资金已动,账目不一致 = 资损或对账亏空)

def check(ctx):
    diffs = []
    txs = ctx.scan("payment-channel", "acquirer_tx", 5000)

    for tx in txs:
        status = tx.after.get("status", "")
        if status != "succeeded":
            continue
        pi_id = tx.after.get("pi_id") or tx.after.get("payment_intent_id")
        if not pi_id:
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        ledger = related.find("accounting-system", "ledger_entry")

        if ledger == None or ledger.is_empty():
            diffs.append({
                "type": "channel_no_accounting_leg",
                "key": pi_id,
                "detail": {
                    "tx_id": tx.after.get("id") or tx.pk,
                    "amount": tx.after.get("amount"),
                    "currency": tx.after.get("currency"),
                    "channel_succeeded_at": tx.after.get("succeeded_at") or tx.after.get("updated_at"),
                    "verdict": "orphan",
                    "severity": "critical",
                    "hint": "渠道已扣款但 accounting 未记账 — outbox 未投递或 consumer 卡住",
                },
            })
    return diffs
