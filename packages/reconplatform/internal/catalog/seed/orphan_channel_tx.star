# orphan_channel_tx — 三方对账 (5/8): 孤儿 channel tx.
# meta-version: 1
#
# payment-channel 有 succeeded tx,但 order-core 找不到 PI.
#
# 触发场景:
#   - 渠道侧主动入账 (银行直入 / 离线对账文件) 但 order-core 无 PI
#   - PI 写入 order-core 失败但渠道已成功
#   - 重复 webhook 投递创建了重复 tx
#
# 严重度: warning (有钱但找不到源,业务侧需介入认领)

def check(ctx):
    diffs = []
    txs = ctx.scan("payment-channel", "acquirer_tx")

    for tx in txs:
        if tx.str("status") != "succeeded":
            continue
        pi_id = tx.str("pi_id") or tx.str("payment_intent_id")

        if pi_id == "":
            # 渠道甚至没填 pi_id, 更严重
            diffs.append({
                "type": "channel_tx_no_pi_id",
                "key":  tx.pk,
                "detail": {
                    "tx_id":  tx.str("id"),
                    "amount": tx.int("amount"),
                    "verdict":  "orphan",
                    "severity": "critical",
                    "hint": "渠道 tx 缺 pi_id — 上游 SDK / webhook 协议异常",
                },
            })
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        pi = related.find("order-core", "payment_intent")
        if not pi:
            diffs.append({
                "type": "orphan_channel_tx",
                "key": pi_id,
                "detail": {
                    "tx_id":      tx.str("id") or tx.pk,
                    "amount":     tx.int("amount"),
                    "currency":   tx.str("currency"),
                    "channel_at": tx.str("created_at") or tx.str("succeeded_at"),
                    "verdict":  "orphan",
                    "severity": "warning",
                    "hint": "渠道有 tx 但 order-core 无 PI — 检查 PI 写入失败或乱序",
                },
            })
    return diffs
