# Chaos Engineering — 故障注入实验

3 个核心实验验证降级 / 重试 / 恢复路径。每个实验都有 **steady-state hypothesis** (稳态假设): 注入故障后系统应该仍能服务用户 (可能降级、不能崩)。

## 前置

```bash
# 安装 chaos-mesh
curl -sSL https://mirrors.chaos-mesh.org/v2.6.3/install.sh | bash -s -- --local kind
# 或 helm:
helm install chaos-mesh chaos-mesh/chaos-mesh -n chaos-mesh --create-namespace
```

## 实验清单

| 实验 | 目标 | Steady-state | 期望表现 |
|---|---|---|---|
| `network-loss-payment-channel.yaml` | payment-channel → 外部渠道 50% 丢包 5min | 退款成功率 > 90% | 重试 + bulkhead 兜底，p99 上升但不超 SLO |
| `pod-kill-rolling.yaml` | 每 30s 随机杀一个 service replica, 10min | HTTP 5xx < 1% | HPA + 多副本无感切换 |
| `db-stall-primary.yaml` | 主库延迟 1s 持续 3min | 不写 / 不变慢的请求继续工作 | 异步 worker pause, 同步路径走 read replica |

## 跑

```bash
# 1. 单个实验
kubectl apply -f deploy/chaos/network-loss-payment-channel.yaml

# 2. 看实验状态
kubectl get networkchaos -A
kubectl describe networkchaos network-loss-payment-channel -n chaos-mesh

# 3. 期间盯指标
# - Grafana dashboard "Payment Platform SLO"
# - 告警是否触发: alertmanager 应该收到 P2 通知
# - synthetic probe 看 SLA

# 4. 清理
kubectl delete -f deploy/chaos/network-loss-payment-channel.yaml
```

## GameDay 流程

每月一次 (建议第一个周二 14:00):

1. **Plan** — 选 1 个实验, 在 #incidents Slack 频道公告
2. **Hypothesis** — 写下"期望系统能…" (RPO/RTO 数字)
3. **Run** — kubectl apply 实验
4. **Observe** — 关注 5 个面板: error rate, latency p99, saturation, traffic, errors
5. **Verify** — 系统恢复后核对 invariants (资金对账 / 订单状态机)
6. **Postmortem** — 写下: 命中什么、没命中什么、修什么

## 灾备 RTO/RPO 目标

| 场景 | RTO (恢复) | RPO (数据丢失) |
|---|---|---|
| 单 pod 死 | 30s | 0 |
| 整 node 死 | 5min | 0 |
| 单 region AZ 故障 | 15min | 0 (Kafka replication factor=3) |
| 整 region 死 | 4h | 1h (S3 backup + binlog) |
| 数据库主库挂 | 1min | 0 (semi-sync replication) |
| KMS 短暂不可用 | 服务降级,token cache 续 1h | 0 |

详见 `docs/DR_PLAN.md`.
