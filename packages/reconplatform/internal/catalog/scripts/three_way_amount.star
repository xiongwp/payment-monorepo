# three_way_amount — 三方对账之 (3):
#
# order-core.payment_intent.amount
#   ==
# payment-channel.acquirer_tx.amount
#   ==
# accounting-system.ledger_entry.amount
#
# 任一不等 → 报 diff,资金可能流向错误账户或多/少扣。
#
# 严重度: critical (金额不等于直接资损)

def check(ctx):
    diffs = []
    intents = ctx.scan("order-core", "payment_intent", 5000)

    for pi in intents:
        if pi.after.get("status") != "succeeded":
            continue
        pi_id = pi.after.get("id") or pi.pk
        if not pi_id:
            continue

        pi_amount = pi.after.get("amount")
        if pi_amount == None:
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        tx = related.find("payment-channel", "acquirer_tx")
        ledger = related.find("accounting-system", "ledger_entry")

        # 任一方缺失 -> 由 order_in_channel / channel_in_accounting 报,这里只比金额
        if tx == None or tx.is_empty() or ledger == None or ledger.is_empty():
            continue

        tx_amount = tx.after.get("amount")
        ledger_amount = ledger.after.get("amount")

        if tx_amount != pi_amount or ledger_amount != pi_amount:
            diffs.append({
                "type": "three_way_amount_mismatch",
                "key": pi_id,
                "want": pi_amount,
                "got": {
                    "order": pi_amount,
                    "channel": tx_amount,
                    "accounting": ledger_amount,
                },
                "detail": {
                    "currency": pi.after.get("currency"),
                    "max_delta": max(abs(tx_amount - pi_amount), abs(ledger_amount - pi_amount)),
                    "verdict": "mismatched",
                    "severity": "critical",
                },
            })
    return diffs
