# fee_mismatch — 内部计算 fee 与 channel 上报 fee 差 > {{.tolerance_minor}} 分。

def check(ctx):
    diffs = []
    charges = ctx.scan("payment-channel", "acquirer_tx")
    for ch in charges:
        internal_fee = int(ch.get("internal_fee_minor", 0))
        channel_fee = int(ch.get("channel_fee_minor", 0))
        if internal_fee == 0 and channel_fee == 0:
            continue  # 没费用数据
        delta = abs(internal_fee - channel_fee)
        if delta > {{.tolerance_minor}}:
            diffs.append({
                "type": "fee_mismatch",
                "key": ch.get("id", ""),
                "want": internal_fee,
                "got": channel_fee,
                "detail": {
                    "delta_minor": delta,
                    "adapter": ch.get("adapter", ""),
                    "pi_id": ch.get("pi_id", ""),
                    "trace_id": ch.get("trace_id", ""),
                },
            })
    return diffs
