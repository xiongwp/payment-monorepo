# refund_excess — 某 charge 累计 refund > 原 charge.amount。
#
# 严重问题：refund 失控，可能业务漏检 already-refunded 状态。

def check(ctx):
    diffs = []
    by_charge = {}
    refunds = ctx.scan("order-core", "refund")
    for rf in refunds:
        ch_id = rf.get("charge_id")
        if not ch_id:
            continue
        if rf.get("status") not in ("succeeded", "completed"):
            continue
        by_charge[ch_id] = by_charge.get(ch_id, 0) + int(rf.get("amount", 0))
    for ch_id, refund_total in by_charge.items():
        ch = ctx.get("payment-channel", "acquirer_tx", ch_id)
        if not ch:
            continue
        ch_amount = int(ch.get("amount", 0))
        if refund_total > ch_amount:
            diffs.append({
                "type": "refund_excess",
                "key": ch_id,
                "want": ch_amount,
                "got": refund_total,
                "detail": {
                    "excess_minor": refund_total - ch_amount,
                    "trace_id": ch.get("trace_id", ""),
                    "pi_id": ch.get("pi_id", ""),
                },
            })
    return diffs
