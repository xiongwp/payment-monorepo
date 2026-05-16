# three_way_amount — 三方对账 (3/8): 金额相等校验.
# meta-version: 1
#
# 三方金额必须严格相等 (分位整数):
#   order-core.payment_intent.amount
#     ==
#   payment-channel.acquirer_tx.amount
#     ==
#   accounting-system.ledger_entry.amount
#
# 任一不等 → 资损 (用户多/少扣 或 平台错收 / 漏收).
#
# 严重度: critical (金额不等于直接资损)
#
# 容差:严格 0 (生产货币用整数分位,不该有浮点误差).
# 若需要 ε 容忍,用 ctx.params["tolerance_minor"].

def check(ctx):
    diffs = []
    tolerance = int(ctx.params.get("tolerance_minor", "0"))
    intents = ctx.scan("order-core", "payment_intent")

    for pi in intents:
        if pi.str("status") != "succeeded":
            continue
        pi_id = pi.str("id") or pi.pk
        if pi_id == "":
            continue

        pi_amount = pi.int("amount")
        if pi_amount == 0:
            # 0 元交易跳过 (非典型,且 0==0 没价值)
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        tx = related.find("payment-channel", "acquirer_tx")
        ledger = related.find("accounting-system", "ledger_entry")

        # 任一方缺失 -> 由 order_in_channel / channel_in_accounting 报,这里只比金额
        if not tx or not ledger:
            continue

        tx_amount = tx.int("amount")
        ledger_amount = abs(ledger.int("amount"))   # ledger 借方可能负数

        d_tx = abs(tx_amount - pi_amount)
        d_lg = abs(ledger_amount - pi_amount)
        max_delta = d_tx if d_tx > d_lg else d_lg

        if max_delta > tolerance:
            diffs.append({
                "type": "three_way_amount_mismatch",
                "key": pi_id,
                "want": pi_amount,
                "got": {
                    "order":      pi_amount,
                    "channel":    tx_amount,
                    "accounting": ledger_amount,
                },
                "detail": {
                    "currency":  pi.str("currency"),
                    "max_delta": max_delta,
                    "verdict":   "mismatched",
                    "severity":  "critical",
                },
            })
    return diffs
