# settlement_short_long — 渠道结算单 vs 内部 charge 累加差异。
#
# 外部源 {{.external_source}} 必须先在 external.sources 里注册并跑过 import。
# 容忍差值 {{.tolerance_minor}} 分。

def check(ctx):
    diffs = []
    # 拿外部结算单（按 settlement_date 索引最近一天）
    settles = ctx.get_by_index("source", "{{.external_source}}", source="external")
    by_pi = {}
    for s in settles:
        pi_id = s.get("pi_id")
        if not pi_id:
            continue
        by_pi[pi_id] = by_pi.get(pi_id, 0) + int(s.get("amount", 0))

    # 拿内部 charge（最近 24h），累加 amount
    internal_by_pi = {}
    charges = ctx.scan("payment-channel", "acquirer_tx")
    for ch in charges:
        pi_id = ch.get("pi_id")
        if not pi_id:
            continue
        internal_by_pi[pi_id] = internal_by_pi.get(pi_id, 0) + int(ch.get("amount", 0))

    # 比对
    all_pi_ids = set(by_pi.keys()) | set(internal_by_pi.keys())
    for pi_id in all_pi_ids:
        ext = by_pi.get(pi_id, 0)
        intr = internal_by_pi.get(pi_id, 0)
        delta = ext - intr
        if abs(delta) > {{.tolerance_minor}}:
            diffs.append({
                "type": "settlement_short_long",
                "key": pi_id,
                "want": intr,
                "got": ext,
                "detail": {
                    "delta_minor": delta,
                    "direction": "long" if delta > 0 else "short",
                    "source": "{{.external_source}}",
                },
            })
    return diffs
