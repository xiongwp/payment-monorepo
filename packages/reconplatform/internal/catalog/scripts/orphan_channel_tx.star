# orphan_channel_tx — 三方对账之 (5):
#
# payment-channel 有 acquirer_tx (succeeded),但 order-core 找不到对应 PI.
#
# 触发场景:
#   - 渠道侧主动入账 (银行直入) 但 order-core 没有对应 PI
#   - PI 写入 order-core 失败但渠道已成功
#   - 重复 webhook 投递创建了重复 tx
#
# 严重度: warning (有钱但找不到源,业务侧需介入认领)

def check(ctx):
    diffs = []
    txs = ctx.scan("payment-channel", "acquirer_tx", 5000)

    for tx in txs:
        if tx.after.get("status") != "succeeded":
            continue
        pi_id = tx.after.get("pi_id") or tx.after.get("payment_intent_id")
        if not pi_id:
            # 渠道甚至没填 pi_id,更严重
            diffs.append({
                "type": "channel_tx_no_pi_id",
                "key": tx.pk,
                "detail": {
                    "tx_id": tx.after.get("id"),
                    "amount": tx.after.get("amount"),
                    "verdict": "orphan",
                    "severity": "critical",
                    "hint": "渠道 tx 缺 pi_id — 上游 SDK / webhook 协议异常",
                },
            })
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        pi = related.find("order-core", "payment_intent")
        if pi == None or pi.is_empty():
            diffs.append({
                "type": "orphan_channel_tx",
                "key": pi_id,
                "detail": {
                    "tx_id": tx.after.get("id") or tx.pk,
                    "amount": tx.after.get("amount"),
                    "currency": tx.after.get("currency"),
                    "channel_at": tx.after.get("created_at"),
                    "verdict": "orphan",
                    "severity": "warning",
                    "hint": "渠道有 tx 但 order-core 无 PI — 检查 PI 写入失败或乱序",
                },
            })
    return diffs
