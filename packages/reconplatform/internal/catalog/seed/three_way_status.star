# three_way_status — 三方对账 (4/8): 状态一致性.
# meta-version: 1
#
# PI 终态 (succeeded / failed / canceled) 应当与 channel + accounting 一致.
# 失配组合 (任一即 diff):
#
#   order.succeeded  + channel.failed     -> 不应成功 (UI 误导)
#   order.failed     + channel.succeeded  -> 用户被扣但 PI 失败 (退款风险)
#   order.canceled   + channel.succeeded  -> 已取消但渠道仍下账 (void/refund 缺失)
#
# 严重度: critical (状态不一致 = 用户体验 + 资金风险)

def check(ctx):
    diffs = []
    intents = ctx.scan("order-core", "payment_intent")
    TERMINAL = ["succeeded", "failed", "canceled"]

    for pi in intents:
        pi_status = pi.str("status")
        if pi_status not in TERMINAL:
            continue
        pi_id = pi.str("id") or pi.pk
        if pi_id == "":
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        tx = related.find("payment-channel", "acquirer_tx")
        if not tx:
            continue   # 缺失由 order_in_channel 报,这里只看不一致

        tx_status = tx.str("status")

        sub = ""
        hint = ""
        if pi_status == "succeeded" and (tx_status == "failed" or tx_status == "declined"):
            sub  = "order_says_succeeded_channel_failed"
            hint = "PI 显示成功但渠道实际失败 — UI 误导用户"
        elif pi_status == "failed" and tx_status == "succeeded":
            sub  = "user_charged_but_failed"
            hint = "用户被扣款但 PI 失败 — 应触发自动退款"
        elif pi_status == "canceled" and tx_status == "succeeded":
            sub  = "canceled_but_captured"
            hint = "PI 已取消但渠道仍捕获 — 立刻发起 void/refund"

        if sub != "":
            diffs.append({
                "type": "three_way_status_mismatch",
                "key": pi_id,
                "detail": {
                    "subtype":        sub,
                    "order_status":   pi_status,
                    "channel_status": tx_status,
                    "amount":         pi.int("amount"),
                    "verdict":  "mismatched",
                    "severity": "critical",
                    "hint": hint,
                },
            })
    return diffs
