# webhook_lag — channel 上报 ack 与 webhook_raw 落库时间差超 {{.threshold_seconds}}s。

def check(ctx):
    diffs = []
    threshold_ms = {{.threshold_seconds}} * 1000
    webhooks = ctx.scan("payment-channel", "webhook_raw")
    for wh in webhooks:
        recv_ms = int(wh.get("received_at_ms", 0))
        ack_ms = int(wh.get("acked_at_ms", 0))
        if recv_ms == 0 or ack_ms == 0:
            continue
        lag = ack_ms - recv_ms
        if lag > threshold_ms:
            diffs.append({
                "type": "webhook_lag",
                "key": wh.get("id", ""),
                "detail": {
                    "lag_seconds": int(lag / 1000),
                    "adapter": wh.get("adapter", ""),
                    "trace_id": wh.get("trace_id", ""),
                },
            })
    return diffs
