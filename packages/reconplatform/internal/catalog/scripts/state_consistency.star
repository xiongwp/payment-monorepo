# state_consistency — payment_intent.status vs charge.status 不该共存的组合。
#
# 状态机一致性规则:
#   PI=succeeded → 必须有 charge.status in (succeeded, captured)
#   PI=canceled  → 不应有 charge.status=succeeded
#   PI=processing → charge.status 不能 succeeded（除非 PI 也升级了）

INVALID_COMBOS = {
    ("succeeded", "failed"): "PI succeeded but charge failed",
    ("succeeded", "canceled"): "PI succeeded but charge canceled",
    ("canceled", "succeeded"): "PI canceled but charge succeeded",
    ("canceled", "captured"): "PI canceled but charge captured",
    ("requires_payment_method", "succeeded"): "PI not started but charge succeeded",
}

def check(ctx):
    diffs = []
    pis = ctx.scan("order-core", "payment_intent")
    for pi in pis:
        pi_status = pi.get("status", "")
        charges = ctx.get_by_index("pi_id", pi.get("id", ""))
        for ch in charges.find_all("payment-channel", "acquirer_tx"):
            ch_status = ch.get("status", "")
            combo = (pi_status, ch_status)
            if combo in INVALID_COMBOS:
                diffs.append({
                    "type": "state_consistency",
                    "key": pi.get("id", ""),
                    "detail": {
                        "pi_status": pi_status,
                        "charge_status": ch_status,
                        "charge_id": ch.get("id", ""),
                        "reason": INVALID_COMBOS[combo],
                        "trace_id": pi.get("trace_id", ""),
                    },
                })
    return diffs
