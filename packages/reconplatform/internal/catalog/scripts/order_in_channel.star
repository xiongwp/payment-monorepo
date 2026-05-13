# order_in_channel — 三方对账之 (1):
#
# order-core 创建的 PaymentIntent (PI) 是否在 payment-channel 落了 acquirer_tx?
#
# 触发场景:
#   - 收银台返成功了但 channel 路由失败 (前端误以为完成)
#   - PI 超时还没下发渠道 (router 卡死)
#   - channel webhook 丢了 PI 一直 pending
#
# 严重度: critical (用户已经 "支付成功" 看起来,实际钱没扣)
#
# 输出 diff.key = pi_id;detail 含 status / 滞留时长 / acquirer 期望渠道.

def check(ctx):
    diffs = []
    intents = ctx.scan("order-core", "payment_intent", 5000)

    for pi in intents:
        pi_status = pi.after.get("status", "")
        # 只关心已确认应推渠道的状态
        if pi_status not in ("processing", "succeeded"):
            continue

        pi_id = pi.after.get("id") or pi.pk
        if not pi_id:
            continue

        # 跨服务查这笔 PI 在 channel 的 acquirer_tx
        related = ctx.get_by_index("pi_id", pi_id)
        tx = related.find("payment-channel", "acquirer_tx")

        if tx == None or tx.is_empty():
            diffs.append({
                "type": "order_no_channel_tx",
                "key": pi_id,
                "detail": {
                    "pi_status": pi_status,
                    "amount": pi.after.get("amount"),
                    "currency": pi.after.get("currency"),
                    "merchant_id": pi.after.get("merchant_id"),
                    "created_at": pi.after.get("created_at"),
                    "verdict": "orphan",
                    "severity": "critical",
                    "hint": "PI 显示 succeeded 但 channel 没记账 — 可能 channel 路由失败 / webhook 丢",
                },
            })
    return diffs
