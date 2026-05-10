# duplicate_charge — 同一 idempotency_key 在 channel 落多条 charge。
#
# 严重问题：用户被重复扣款。upstream 重试不当 / idempotency 检查漏。

def check(ctx):
    diffs = []
    seen = {}
    charges = ctx.scan("payment-channel", "acquirer_tx")
    for ch in charges:
        idk = ch.get("idempotency_key")
        if not idk:
            continue
        if idk in seen:
            seen[idk].append(ch.get("id"))
        else:
            seen[idk] = [ch.get("id")]
    for idk, ids in seen.items():
        if len(ids) > 1:
            diffs.append({
                "type": "duplicate_charge",
                "key": idk,
                "detail": {
                    "charge_ids": ids,
                    "count": len(ids),
                },
            })
    return diffs
