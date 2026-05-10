# missing_charge_leg — PI 进 succeeded 但 channel 没 charge 落地。
#
# PI succeed 后 {{.min_age_min}}min 还无 charge → 报。

def check(ctx):
    diffs = []
    cutoff_ms = ctx.now_ms - {{.min_age_min}} * 60 * 1000
    pis = ctx.get_by_index("status", "succeeded")
    for pi in pis.find("order-core", "payment_intent"):
        # 太新的 PI 跳过（可能 charge 还在路上）
        if int(pi.get("updated_at_ms", 0)) > cutoff_ms:
            continue
        # 找对应 charge_id（通过 pi_id 索引）
        charges = ctx.get_by_index("pi_id", pi["id"])
        ch_count = len(charges.find("payment-channel", "acquirer_tx"))
        if ch_count == 0:
            diffs.append({
                "type": "missing_charge_leg",
                "key": pi["id"],
                "detail": {
                    "trace_id": pi.get("trace_id", ""),
                    "amount": pi.get("amount", 0),
                    "currency": pi.get("currency", ""),
                    "merchant_id": pi.get("merchant_id", ""),
                    "pi_age_minutes": int((ctx.now_ms - int(pi.get("updated_at_ms", 0))) / 60000),
                },
            })
    return diffs
