# orphan_accounting_entry — 三方对账之 (6):
#
# accounting-system 有 ledger_entry,但渠道侧 (payment-channel) 找不到对应 tx.
#
# 触发场景:
#   - 手工调账 (admin 直接改账)
#   - 多个 outbox 重放导致重复 ledger
#   - accounting-system 误从其他 service 收到事件
#
# 严重度: warning (账目记了但找不到资金来源,人工核对必需)

def check(ctx):
    diffs = []
    entries = ctx.scan("accounting-system", "ledger_entry", 5000)

    for le in entries:
        # 仅关心 charge 类的 ledger (refund / fee 由其他规则管)
        biz_type = le.after.get("business_type") or le.after.get("source")
        if biz_type not in ("charge", "payment", "deposit"):
            continue

        pi_id = le.after.get("pi_id") or le.after.get("payment_intent_id")
        if not pi_id:
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        tx = related.find("payment-channel", "acquirer_tx")

        if tx == None or tx.is_empty():
            diffs.append({
                "type": "orphan_accounting_entry",
                "key": pi_id,
                "detail": {
                    "ledger_id": le.after.get("id") or le.pk,
                    "amount": le.after.get("amount"),
                    "voucher_no": le.after.get("voucher_no"),
                    "actor": le.after.get("created_by") or "auto",
                    "verdict": "orphan",
                    "severity": "warning",
                    "hint": "账目存在但渠道无对应 tx — 可能手工调账 / 重复 outbox 投递",
                },
            })
    return diffs
