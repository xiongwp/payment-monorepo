# three_way_sync_lag — 三方对账之 (8):
#
# 单方数据已到,但其他方在 SLA 时间窗内仍未到位 (区别于真"丢失",这是 lag).
#
# SLA 假设 (可由 ctx.params 调整):
#   - channel.succeeded 后 10s 内 order PI 应该到 succeeded
#   - accounting.ledger_entry 应该 30s 内入账
#
# 触发场景: Kafka MM lag / consumer 卡 / 重启抖动.
#
# 严重度: warning (短窗,但持续放大 → escalate)

def check(ctx):
    diffs = []
    # 可配参
    order_sla_sec = int(ctx.params.get("order_sla_sec", "10"))
    accounting_sla_sec = int(ctx.params.get("accounting_sla_sec", "30"))
    now = ctx.now_ts()

    txs = ctx.scan("payment-channel", "acquirer_tx", 5000)
    for tx in txs:
        if tx.after.get("status") != "succeeded":
            continue
        pi_id = tx.after.get("pi_id") or tx.after.get("payment_intent_id")
        if not pi_id:
            continue

        succeeded_at = _parse_ts(tx.after.get("succeeded_at") or tx.after.get("updated_at"))
        if succeeded_at == 0:
            continue
        age = now - succeeded_at

        # 还在 SLA 窗口内,跳过;够老才报
        if age < order_sla_sec:
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        pi = related.find("order-core", "payment_intent")
        ledger = related.find("accounting-system", "ledger_entry")

        lagging = []
        if pi != None and not pi.is_empty():
            if pi.after.get("status") not in ("succeeded", "failed"):
                lagging.append({"svc": "order-core", "current_status": pi.after.get("status")})
        if (ledger == None or ledger.is_empty()) and age > accounting_sla_sec:
            lagging.append({"svc": "accounting-system", "current_status": "missing"})

        if len(lagging) > 0:
            diffs.append({
                "type": "three_way_sync_lag",
                "key": pi_id,
                "detail": {
                    "channel_succeeded_at": tx.after.get("succeeded_at"),
                    "age_sec": age,
                    "lagging": lagging,
                    "verdict": "pending" if age < accounting_sla_sec * 3 else "mismatched",
                    "severity": "warning" if age < accounting_sla_sec * 5 else "critical",
                    "hint": "渠道已完成 " + str(age) + "s 但下游未同步",
                },
            })
    return diffs


def _parse_ts(s):
    # 简化:用 ctx.parse_ts 兜底,starlark 这里返 0 让 caller 跳过
    if s == None or s == "":
        return 0
    # 若 ctx 提供了 helper:
    if hasattr(globals(), "ts_of"):
        return ts_of(s)
    return 0
