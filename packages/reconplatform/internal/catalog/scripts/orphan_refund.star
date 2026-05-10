# orphan_refund — refund 找不到对应 charge_id（孤儿 refund）。
#
# 数据完整性问题：refund 必须挂在某个 charge 下面。

def check(ctx):
    diffs = []
    refunds = ctx.scan("order-core", "refund")
    for rf in refunds:
        ch_id = rf.get("charge_id")
        if not ch_id:
            diffs.append({
                "type": "orphan_refund",
                "key": rf.get("id", ""),
                "detail": {"reason": "refund.charge_id is empty"},
            })
            continue
        charge = ctx.get("payment-channel", "acquirer_tx", ch_id)
        if not charge:
            diffs.append({
                "type": "orphan_refund",
                "key": rf.get("id", ""),
                "detail": {
                    "charge_id": ch_id,
                    "amount": rf.get("amount", 0),
                    "trace_id": rf.get("trace_id", ""),
                    "reason": "charge not found by charge_id",
                },
            })
    return diffs
