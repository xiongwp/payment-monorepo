# refund_three_way — 三方对账 (7/8): 退款链路三方对账.
# meta-version: 1
#
# 退款链路:
#   order-core.refund_request -> payment-channel.refund -> accounting-system.reverse_entry
#
# 三方都到齐 + 金额相等才算正确退款.
#
# 严重度: critical (退款失败用户投诉激增,且资金风险)

def check(ctx):
    diffs = []
    refunds = ctx.scan("order-core", "refund_request")

    for rf in refunds:
        rf_status = rf.str("status")
        if rf_status != "processing" and rf_status != "succeeded":
            continue
        refund_id = rf.str("id") or rf.pk
        pi_id = rf.str("pi_id") or rf.str("payment_intent_id")
        if pi_id == "" or refund_id == "":
            continue

        rf_amount = rf.int("amount")

        # 优先按 refund_id 找,没建索引就 fall back 到 pi_id
        related = ctx.get_by_index("refund_id", refund_id)
        if related.len == 0:
            related = ctx.get_by_index("pi_id", pi_id)

        chan_refund  = related.find("payment-channel", "refund")
        acct_reverse = related.find("accounting-system", "ledger_entry")

        missing = []
        if not chan_refund:
            missing.append("payment-channel")
        if not acct_reverse:
            missing.append("accounting-system")

        if len(missing) > 0:
            diffs.append({
                "type": "refund_leg_missing",
                "key":  refund_id,
                "detail": {
                    "pi_id":          pi_id,
                    "refund_amount":  rf_amount,
                    "order_status":   rf_status,
                    "missing_in":     missing,
                    "verdict":  "orphan",
                    "severity": "critical",
                    "hint": "退款未在缺失方完成 — 资金未真正退回",
                },
            })
            continue

        # 三方都到齐 -> 比金额
        chan_amount = chan_refund.int("amount")
        acct_amount = abs(acct_reverse.int("amount"))
        if rf_amount != chan_amount or rf_amount != acct_amount:
            diffs.append({
                "type": "refund_amount_mismatch",
                "key":  refund_id,
                "want": rf_amount,
                "got": {
                    "order":      rf_amount,
                    "channel":    chan_amount,
                    "accounting": acct_amount,
                },
                "detail": {
                    "pi_id":    pi_id,
                    "verdict":  "mismatched",
                    "severity": "critical",
                },
            })
    return diffs
