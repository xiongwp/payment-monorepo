# refund_three_way — 三方对账之 (7):
#
# 退款链路:
#   order-core.refund_request -> payment-channel.refund -> accounting-system.reverse_entry
#
# 三方都到齐 + 金额相等才算正确退款.
#
# 严重度: critical (退款失败用户投诉激增,资金风险)

def check(ctx):
    diffs = []
    refunds = ctx.scan("order-core", "refund_request", 3000)

    for rf in refunds:
        rf_status = rf.after.get("status", "")
        # 已发起的退款才检查
        if rf_status not in ("processing", "succeeded"):
            continue
        refund_id = rf.after.get("id") or rf.pk
        pi_id = rf.after.get("pi_id") or rf.after.get("payment_intent_id")
        if not pi_id or not refund_id:
            continue

        rf_amount = rf.after.get("amount")
        related = ctx.get_by_index("refund_id", refund_id) or ctx.get_by_index("pi_id", pi_id)

        chan_refund = related.find("payment-channel", "refund")
        acct_reverse = related.find("accounting-system", "ledger_entry")

        # 三方都缺?
        missing = []
        if chan_refund == None or chan_refund.is_empty():
            missing.append("payment-channel")
        if acct_reverse == None or acct_reverse.is_empty():
            missing.append("accounting-system")

        if len(missing) > 0:
            diffs.append({
                "type": "refund_leg_missing",
                "key": refund_id,
                "detail": {
                    "pi_id": pi_id,
                    "refund_amount": rf_amount,
                    "order_status": rf_status,
                    "missing_in": missing,
                    "verdict": "orphan",
                    "severity": "critical",
                    "hint": "退款未在 " + ",".join(missing) + " 完成 — 资金未真正退回",
                },
            })
            continue

        # 金额对比
        chan_amount = chan_refund.after.get("amount")
        acct_amount = abs(acct_reverse.after.get("amount", 0))
        if rf_amount != chan_amount or rf_amount != acct_amount:
            diffs.append({
                "type": "refund_amount_mismatch",
                "key": refund_id,
                "want": rf_amount,
                "got": {
                    "order": rf_amount,
                    "channel": chan_amount,
                    "accounting": acct_amount,
                },
                "detail": {
                    "pi_id": pi_id,
                    "verdict": "mismatched",
                    "severity": "critical",
                },
            })
    return diffs
