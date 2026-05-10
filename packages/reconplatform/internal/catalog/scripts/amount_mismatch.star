# amount_mismatch — order-core PaymentIntent.amount 与 accounting account_transaction sum 差异。
#
# 适用：每 15min 检查最近 1h 内 succeed 的 payment_intent，看会计入账金额是否吻合。
# 容忍差值: {{.tolerance_minor}} 分（minor unit）。

def check(ctx):
    diffs = []
    succeeded = ctx.get_by_index("status", "succeeded")
    for pi in succeeded.find("order-core", "payment_intent"):
        if not pi.get("id"):
            continue
        # 累加 accounting 里的 transaction.amount，related_pi_id 关联
        txs = ctx.get_by_index("related_pi_id", pi["id"])
        total = 0
        for tx in txs.find("accounting-system", "account_transaction"):
            # signed amount: debit/credit 通过 business_type 判方向
            total += int(tx.get("amount", 0))
        diff = total - int(pi.get("amount", 0))
        if abs(diff) > {{.tolerance_minor}}:
            diffs.append({
                "type": "amount_mismatch",
                "key": pi["id"],
                "want": pi["amount"],
                "got": total,
                "detail": {
                    "trace_id": pi.get("trace_id", ""),
                    "merchant_id": pi.get("merchant_id", ""),
                    "tx_count": len(txs),
                },
            })
    return diffs
