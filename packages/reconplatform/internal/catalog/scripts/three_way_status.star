# three_way_status — 三方对账之 (4):
#
# 状态一致性: PI 终态 (succeeded / failed / canceled) 应当与 channel + accounting 一致.
#
# 矩阵 (任一组合即为 diff):
#   order.succeeded  + channel.failed     -> 不应该成功
#   order.failed     + channel.succeeded  -> 用户被扣了但平台显示失败 (退款风险)
#   order.canceled   + channel.succeeded  -> 已取消但渠道仍下账
#   order.succeeded  + accounting 无记账  -> 由 channel_in_accounting 捕
#
# 严重度: critical (状态不一致 = 用户体验 + 资金风险)

def check(ctx):
    diffs = []
    intents = ctx.scan("order-core", "payment_intent", 5000)

    OK_TERMINAL = ("succeeded", "failed", "canceled")

    for pi in intents:
        pi_status = pi.after.get("status", "")
        if pi_status not in OK_TERMINAL:
            continue
        pi_id = pi.after.get("id") or pi.pk
        if not pi_id:
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        tx = related.find("payment-channel", "acquirer_tx")
        if tx == None or tx.is_empty():
            continue

        tx_status = tx.after.get("status", "")

        # 状态不一致的几种 critical 组合
        if pi_status == "succeeded" and tx_status in ("failed", "declined"):
            diffs.append(_mk_status_diff(pi_id, pi_status, tx_status,
                "order_says_succeeded_channel_failed",
                "PI 显示成功但渠道实际失败 — UI 误导用户"))
        elif pi_status == "failed" and tx_status == "succeeded":
            diffs.append(_mk_status_diff(pi_id, pi_status, tx_status,
                "user_charged_but_failed",
                "用户被扣款但 PI 显示失败 — 应自动触发退款"))
        elif pi_status == "canceled" and tx_status == "succeeded":
            diffs.append(_mk_status_diff(pi_id, pi_status, tx_status,
                "canceled_but_captured",
                "PI 已取消但渠道仍捕获 — 立刻发起 void/refund"))

    return diffs


def _mk_status_diff(pi_id, pi_status, tx_status, subtype, hint):
    return {
        "type": "three_way_status_mismatch",
        "key": pi_id,
        "detail": {
            "subtype": subtype,
            "order_status": pi_status,
            "channel_status": tx_status,
            "verdict": "mismatched",
            "severity": "critical",
            "hint": hint,
        },
    }
