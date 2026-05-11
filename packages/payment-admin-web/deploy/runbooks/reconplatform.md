# reconplatform Runbook

## Service Overview
- **Purpose**: 实时对账平台 — CDC + Starlark 脚本 + 6 大企业模块
- **Owner**: finance team
- **SLO**: CDC lag < 60s / P0 diff = 0
- **Port**: :9180

## Common Alerts

### `ReconCDCLagHigh` — binlog lag > 60s
1. 看 admin web Dashboard tab — 各 source runner lag
2. 单 shard 慢: `docker logs reconplatform-admin | grep <shard_name>`
3. 全部慢: Redis 可能慢 (stream 写 backpressure)
4. **Mitigation**:
   - 临时停一些低优先级 source
   - 或扩 reconplatform replica (多消费者)

### `ReconUnresolvedP0`
有 P0 diff 1h+ 没 ack — 资金事故信号
1. admin web Diffs tab 看具体 diff
2. P0 diff 的 detail 一般含 trace_id → Jaeger 看链路
3. 30min 内必须 ack 或转 ops

## Common Ops
```bash
# Live tail binlog 事件
curl -N :9180/api/v1/events/stream

# Trial balance 手工跑
curl -X POST :9180/api/v1/diffs/_external -d '<...>'

# Catalog 装规则
curl :9180/admin/   # UI 操作
```
