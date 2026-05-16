# three_way_sync_lag — 三方对账 (8/8): 同步滞后检测.
# meta-version: 1
#
# 单方数据已到, 但其他方在 SLA 时间窗内仍未到位 (区别于真"丢失", 这是 lag).
#
# SLA (可由 ctx.params 覆盖, 默认):
#   order_sla_sec       = 10s     channel succeeded 后 PI 应推进
#   accounting_sla_sec  = 30s     accounting 应入账
#
# 触发场景: Kafka MM lag / consumer 卡 / 重启抖动.
# 严重度: warning (短窗, 但持续放大 → 自动 escalate 到 critical)

def check(ctx):
    diffs = []
    order_sla_sec = int(ctx.params.get("order_sla_sec", "10"))
    acct_sla_sec  = int(ctx.params.get("accounting_sla_sec", "30"))
    now_ms = ctx.now_ms

    txs = ctx.scan("payment-channel", "acquirer_tx")
    for tx in txs:
        if tx.str("status") != "succeeded":
            continue
        pi_id = tx.str("pi_id") or tx.str("payment_intent_id")
        if pi_id == "":
            continue

        # 渠道成功时间 (ms),期望字段 succeeded_at_ms / updated_at_ms 已是 int
        succeeded_ms = tx.int("succeeded_at_ms") or tx.int("updated_at_ms")
        if succeeded_ms == 0:
            continue
        age_sec = (now_ms - succeeded_ms) // 1000
        if age_sec < order_sla_sec:
            continue   # 还在 SLA 窗内, 不算 lag

        related = ctx.get_by_index("pi_id", pi_id)
        pi      = related.find("order-core", "payment_intent")
        ledger  = related.find("accounting-system", "ledger_entry")

        lagging = []
        if pi:
            pi_status = pi.str("status")
            if pi_status != "succeeded" and pi_status != "failed":
                lagging.append({"svc": "order-core", "current_status": pi_status})
        if not ledger and age_sec > acct_sla_sec:
            lagging.append({"svc": "accounting-system", "current_status": "missing"})

        if len(lagging) > 0:
            verdict  = "pending"
            severity = "warning"
            if age_sec > acct_sla_sec * 3:
                verdict = "mismatched"
            if age_sec > acct_sla_sec * 5:
                severity = "critical"
            diffs.append({
                "type": "three_way_sync_lag",
                "key":  pi_id,
                "detail": {
                    "channel_succeeded_at_ms": succeeded_ms,
                    "age_sec":  age_sec,
                    "lagging":  lagging,
                    "verdict":  verdict,
                    "severity": severity,
                    "hint": "渠道完成 N 秒后下游仍未同步",
                },
            })
    return diffs
