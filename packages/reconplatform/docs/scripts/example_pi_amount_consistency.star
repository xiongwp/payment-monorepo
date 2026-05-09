# Example：PI 三表金额一致性对账（Starlark 版）。
#
# 业务场景：用户支付一笔 100 元，三个表必须金额一致：
#   - order-core.payment_intents.amount
#   - accounting-system.account_transaction.amount  (related_pi_id=PI)
#   - payment-channel.card_charges.amount           (pi_id=PI)
#
# 三方任何一方缺失或金额不一致 → 资损嫌疑，写 diff 给运营人工核查。
#
# 编辑器 autocomplete：
#   - 写 "load(" 时 admin web Monaco 自动弹出 @json/@time/@math/@strings/@regex/@recon
#     6 个 builtin module + RegisterModule 动态注册的 host 包。
#   - 写 "ctx." 时弹 scan_index/get_by_index/get/http_get/log_info 等。
#   - 写 ".find(" 时弹出 service/table 候选（schema 来自 /api/v1/meta/* 端点）。
#
# 触发方式：
#   - manual: admin 点 "Run Now"
#   - cron:   "*/15 * * * *"  (每 15 分钟扫一次最近 24h 的 PI)
#   - stream: ["accounting-system:account_transaction", "payment-channel:card_charges"]
#
# 性能：扫 1h 内 ~5K PI 大概 5K × 3 次 get_by_index = 15K Redis SMEMBERS+MGET，
# 在本地 Redis 上 ~2 秒；admin web "Run Now" 同步等结果体验 OK。

load("@recon", "last_n_hours")

def check(ctx):
    """返 list[dict] — 每条 dict 是一条 diff (type / key / want / got / detail)"""
    diffs = []

    # 扫最近 24 小时所有 pi_id。Starlark 没 unpack-一行赋值，用 tuple unpacking 风格。
    since, _ = last_n_hours(ctx.now, 24)
    pi_ids = ctx.scan_index("pi_id", "", 5000)

    if len(pi_ids) == 0:
        ctx.log_info("no pi_id events in window", "since", since)
        return diffs

    for pi in pi_ids:
        events = ctx.get_by_index("pi_id", pi)
        if events.len == 0:
            # 索引 SET 还没 GC，但主存已过期 → 跳过
            continue

        order = events.find("order-core", "payment_intents")
        txn = events.find("accounting-system", "account_transaction")
        chg = events.find("payment-channel", "card_charges")

        # case 1: 缺腿（任意一方没找到）
        missing = []
        if not order:
            missing.append("order-core/payment_intents")
        if not txn:
            missing.append("accounting-system/account_transaction")
        if not chg:
            missing.append("payment-channel/card_charges")
        if 0 < len(missing) < 3:
            # 全缺意味着事件还没传过来（或者本来就不是支付流），不报；
            # 部分缺才是真问题。
            diffs.append({
                "type": "missing_leg",
                "key": pi,
                "detail": {"missing": missing},
            })
            continue
        if len(missing) == 3:
            continue

        # case 2: 金额不一致
        want = order.int("amount")
        got_txn = txn.int("amount")
        got_chg = chg.int("amount")
        if want != got_txn or want != got_chg:
            diffs.append({
                "type": "amount_mismatch",
                "key": pi,
                "want": want,
                "got": {
                    "order-core/payment_intents": want,
                    "accounting-system/account_transaction": got_txn,
                    "payment-channel/card_charges": got_chg,
                },
            })
            continue

        # case 3: PI succeeded 但 channel 还在 pending（可能 webhook 漏了）
        if order.str("status") == "succeeded" and chg.str("status") != "succeeded":
            diffs.append({
                "type": "pi_succeeded_but_charge_not",
                "key": pi,
                "detail": {
                    "pi_status": order.str("status"),
                    "charge_status": chg.str("status"),
                    "updated_at": chg.str("updated_at"),
                },
            })

        # case 4: PI 创建超过 1h 但 ledger 还没落（accounting 慢）
        # Starlark 没 time.Parse；如需用 load("@time","parse_time") 比 ctx.now 即可
        # 这里简化：依赖事件 timestamp（store 写入时已记录），跳过精确时间窗判断

    ctx.log_info("pi consistency check done", "scanned", len(pi_ids), "diffs", len(diffs))
    return diffs
