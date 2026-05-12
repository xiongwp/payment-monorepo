# Chaos Engineering

chaos-mesh 注入故障验证服务降级 + SLO 不破.

## 部署

```bash
cd infra/chaos
./install.sh
# 装完 dashboard: kubectl port-forward -n chaos-mesh svc/chaos-dashboard 2333:2333
# 浏览器 http://localhost:2333
```

## 实验

| 文件 | 故障 | 验证 |
|---|---|---|
| `01_pod_kill_payment_core.yaml` | 杀 1 个 payment-core pod | HPA reschedule + 业务 5xx<0.5% |
| `02_network_delay_payment_channel.yaml` | 卡组网络 +500ms | 退化路径生效, p99 不爆 |
| `03_mysql_pause.yaml` | shard-0 暂停 1min | outbox backlog 不丢 |
| `04_kafka_partition.yaml` | Kafka 2min 不可达 | outbox 缓冲 + 恢复后追上 |

## 跑单个实验

```bash
kubectl apply -f experiments/01_pod_kill_payment_core.yaml
# 观察 grafana payment-core dashboard
# 清理
kubectl delete podchaos pod-kill-payment-core -n payment
```

## 周度自动跑

```bash
kubectl apply -f experiments/scheduled.yaml
```

每周一 03:00 UTC 跑一个实验, 4 周一循环.

## 跟 SLO 联动

实验跑前: 记下当前 SLO ( golden signal — 5xx rate, p99 latency, error budget).
实验跑中: 同 SLO 监控曲线; 超阈值自动告警 + on-call 上线.
实验跑后: 30min 后比较, 出 markdown 报告 push wiki.

## 黄金信号阈值

| 指标 | 阈值 | 报告 |
|---|---|---|
| 5xx rate | > 1% | warn |
| 5xx rate | > 5% | critical, abort 实验 |
| p99 latency | > 2x baseline | warn |
| error budget burn | 24h budget 消耗 > 5% | abort + 通知 |

## 不能跑的实验 (生产禁止)

- 杀 100% replica 同一服务 (整服务挂)
- 删 db data
- 删 KMS key
- 跨 region 网络 partition (DR 演练专用, 走单独 runbook)
