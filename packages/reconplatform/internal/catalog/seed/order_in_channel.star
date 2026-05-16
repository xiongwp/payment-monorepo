# order_in_channel — 三方对账 (1/8): order → channel 存在性.
# meta-version: 1
#
# 目的:
#   order-core 创建并标记为 processing/succeeded 的 PaymentIntent,
#   是否在 payment-channel 落了对应的 acquirer_tx?
#
# 触发场景:
#   - 收银台返成功了但 channel 路由失败 (前端误以为已支付)
#   - PI 超时还没下发渠道 (router 卡死 / config-center 路由表空)
#   - channel webhook 丢导致 PI 一直 pending
#
# 严重度: critical (UI 显示成功但钱没扣 -> 业务损失 / 投诉)
#
# API 用法参考:
#   pi = ctx.scan("order-core", "payment_intent")        # 列全部 PI
#   pi.get("status")          /  pi.str("status")        # 取列值
#   pi.int("amount")                                      # 取整型
#   related = ctx.get_by_index("pi_id", pi.pk)            # 跨服务关联
#   tx = related.find("payment-channel", "acquirer_tx")   # 找对方
#   if tx: ...                # Truth: 空 event 为 False

def check(ctx):
    diffs = []
    intents = ctx.scan("order-core", "payment_intent")

    for pi in intents:
        status = pi.str("status")
        # 只关心已确认应推渠道的状态
        if status != "processing" and status != "succeeded":
            continue

        pi_id = pi.str("id") or pi.pk
        if pi_id == "":
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        tx = related.find("payment-channel", "acquirer_tx")

        # tx 为空 event (truthy=False) 即未找到
        if not tx:
            diffs.append({
                "type": "order_no_channel_tx",
                "key": pi_id,
                "detail": {
                    "pi_status":   status,
                    "amount":      pi.int("amount"),
                    "currency":    pi.str("currency"),
                    "merchant_id": pi.str("merchant_id"),
                    "created_at":  pi.str("created_at"),
                    "verdict":  "orphan",
                    "severity": "critical",
                    "hint": "PI 显示已成功但 channel 没记账 — 渠道路由 / webhook 异常",
                },
            })
    return diffs
