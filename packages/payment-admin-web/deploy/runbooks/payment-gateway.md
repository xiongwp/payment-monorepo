# payment-gateway Runbook

## Service Overview
- **Purpose**: 商户接入 SDK 唯一入口 — tokenize 卡 + 智能路由 + 统一 charge
- **Owner**: payments team
- **SLO**: 99.99% availability / P99 < 300ms
- **Port**: :18091 / k8s `payment-gateway:8080`

## Dependencies
```
↑ 商户 SDK (Drop-in JS / REST)
↓ payment-channel (具体通道) / kms-manage (PAN 加密)
Effect when down: 商户全部 charge 失败 — 公司 P0 事件
```

## Common Alerts

### `GatewayAvailabilityCritical` — 5xx > 0.01% 5min
1. **第一时间** kubectl logs `-l app=payment-gateway --tail=500 | grep ERROR`
2. 查 `routing.score` 是否所有通道 health 都掉了:
   - `kubectl exec ... curl localhost:8080/api/v1/route -d '<test>'` 看 reasoning
3. KMS 是否慢: prometheus `kms_decrypt_latency_p99`
4. **Mitigation**:
   - 手动加 channel 优先级（admin POST /api/v1/route 设 prefs）
   - 切到 fallback 通道（修 routing 配置）
   - 最坏：临时关 tokenize 走 simple-charge 路径（降级）

### `GatewayLatencyHigh` — P99 > 300ms
1. 查 kms 调用是否慢 (tokenize 流程)
2. 查 channel SDK 调用 p99
3. **Mitigation**: HPA scale-out (`minReplicas: 5 → 10`)

## Common Ops
```bash
# 加新通道
curl -X POST :18091/admin/channels -d '{"id":"new-acquirer","fee_bps":250,...}'

# 强制 channel 健康度重置
curl -X POST :18091/admin/health/reset -d '{"channel_id":"visa-stripe"}'
```

## DR
| 场景 | RTO |
|---|---|
| KMS 挂 | 全部 tokenize 失败 — 必须 KMS 先恢复 |
| 单 channel 全挂 | 自动 fallback，无需介入 |
| 所有 channel 都挂 | 拉外部支持 + 紧急公告 |
