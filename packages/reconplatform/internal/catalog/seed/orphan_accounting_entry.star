# orphan_accounting_entry — 三方对账 (6/8): 孤儿 accounting 账目.
# meta-version: 1
#
# accounting-system 有 ledger_entry, 但渠道侧 (payment-channel) 找不到对应 tx.
#
# 触发场景:
#   - 手工调账 (admin 直接改账目)
#   - 多个 outbox 重放导致重复 ledger
#   - accounting 误从其他 service 收到错事件
#
# 严重度: warning (账目记了但找不到资金来源,人工核对必需)

def check(ctx):
    diffs = []
    entries = ctx.scan("accounting-system", "ledger_entry")

    for le in entries:
        # 仅关心 charge 类 ledger (refund / fee 由别规则管)
        biz = le.str("business_type") or le.str("source")
        if biz != "charge" and biz != "payment" and biz != "deposit":
            continue

        pi_id = le.str("pi_id") or le.str("payment_intent_id")
        if pi_id == "":
            continue

        related = ctx.get_by_index("pi_id", pi_id)
        tx = related.find("payment-channel", "acquirer_tx")

        if not tx:
            diffs.append({
                "type": "orphan_accounting_entry",
                "key": pi_id,
                "detail": {
                    "ledger_id":  le.str("id") or le.pk,
                    "amount":     le.int("amount"),
                    "voucher_no": le.str("voucher_no"),
                    "actor":      le.str("created_by") or "auto",
                    "verdict":  "orphan",
                    "severity": "warning",
                    "hint": "账目存在但渠道无对应 tx — 可能手工调账 / 重复 outbox 投递",
                },
            })
    return diffs
