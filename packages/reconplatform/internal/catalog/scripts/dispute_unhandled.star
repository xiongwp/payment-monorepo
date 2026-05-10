# dispute_unhandled — dispute 状态 received > {{.threshold_hours}}h 未处理。
#
# Dispute 必须及时响应（信用卡组织一般 7-10 天 deadline），超时一律告警。

def check(ctx):
    diffs = []
    threshold_ms = {{.threshold_hours}} * 3600 * 1000
    disputes = ctx.scan("order-core", "dispute")
    for d in disputes:
        if d.get("status") != "received":
            continue
        created_ms = int(d.get("created_at_ms", 0))
        if created_ms == 0:
            continue
        age = ctx.now_ms - created_ms
        if age > threshold_ms:
            diffs.append({
                "type": "dispute_unhandled",
                "key": d.get("id", ""),
                "detail": {
                    "age_hours": int(age / 3600 / 1000),
                    "amount": d.get("amount", 0),
                    "currency": d.get("currency", ""),
                    "reason": d.get("reason", ""),
                    "charge_id": d.get("charge_id", ""),
                    "trace_id": d.get("trace_id", ""),
                },
            })
    return diffs
