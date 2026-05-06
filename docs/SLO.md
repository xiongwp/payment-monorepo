# 支付平台 SLO（Service Level Objectives）

每个服务的可用性 + 延迟目标 + error budget 定义。SRE on-call 看这个判断"是不是该 page"。

## 总体目标

整个支付链路（用户点支付 → 卡组织授权 → 商户拿到回执）：

| 指标 | 目标 | error budget (30d) |
| --- | --- | --- |
| 可用性 | 99.95% | 21.6 min downtime |
| 端到端 P99 延迟 | < 5s | 95% < 5s |
| 资金正确性 | 100%（trial-balance 必平） | 0 |

---

## 各服务 SLO

### api-gateway（用户面 BFF）

| SLI | 目标 | 测量 |
| --- | --- | --- |
| availability | 99.95% | up{job="api-gateway"} OR rate(http_requests_total{status=~"5.."}) / rate(http_requests_total) < 0.0005 |
| latency P99 | < 1s | histogram_quantile(0.99, http_request_duration) |
| latency P50 | < 100ms | 同上 |

**错误预算**：30 天可累计 21.6min 不可用。一次 30min 故障 = 当月预算耗光。

### user-merchant-core

| SLI | 目标 |
| --- | --- |
| availability | 99.95% |
| Login P99 | < 500ms |
| ListCards P99 | < 200ms |
| AttachCard P99 | < 2s（含 card-center 单跳） |

### order-core

| SLI | 目标 |
| --- | --- |
| availability | 99.95% |
| CreatePI P99 | < 300ms |
| Confirm P99 | < 2s（含 payment-channel + card-payment） |
| Refund P99 | < 1s |

### payment-core / payment-channel

| SLI | 目标 |
| --- | --- |
| availability | 99.95% |
| Charge P99 | < 5s（含真渠道 RT） |
| 渠道熔断恢复 | < 1min（HalfOpen 探测 + 3 次成功） |

### card-center / card-payment（PCI 链路）

| SLI | 目标 |
| --- | --- |
| availability | 99.99% |
| Tokenize P99 | < 1s（KMS encrypt + DB） |
| Detokenize P99 | < 500ms |
| Authorize P99 | < 5s（含卡组织 RT） |
| HARD decline rate | < 5% (15min 窗) |
| reconcile lag | < 5min（pending → terminal 状态） |

### accounting-system

| SLI | 目标 |
| --- | --- |
| availability | 99.99% |
| outbox lag | < 30s |
| 复式记账平衡 | 100% |
| sync booking P99 | < 200ms |

### kms-manage

| SLI | 目标 |
| --- | --- |
| availability | 99.99%（PCI 关键路径） |
| Encrypt P99 | < 100ms |
| Decrypt P99 | < 100ms |
| GenerateDataKey P99 | < 200ms |

### risk-manage

| SLI | 目标 |
| --- | --- |
| availability | 99.95%（fail-open 兜底） |
| Screen P99 | < 100ms |
| 熔断不影响主流量（fail-open 默认） | 0 charge 因 risk 不可用而拒 |

---

## Latency budget（端到端切片）

用户支付一次的 5s 总预算如何分配：

```
Browser → api-gateway HTTPS  : 100ms (TLS + LB + 解析)
api-gateway → order-core     : 50ms  (mTLS gRPC LAN)
order-core CreatePI          : 100ms (DB write + 唯一性校验)
order-core → payment-channel : 100ms (mTLS gRPC LAN)
payment-channel → card-payment: 50ms
card-payment Detokenize → card-center: 200ms (含 KMS Decrypt)
card-payment → 卡组织 HTTPS  : 3000ms ← 大头，受外部影响
card-payment 落 DB           : 50ms
回程 ack                     : 同上反向
─────────────────────────────────────
总计                         : ~3.7s 期望，留 1.3s 余量给抖动
```

**任一跳超 budget 一倍**就告警，不必等总超时。

---

## Error Budget Policy

每月初重置 error budget。budget 烧光时：

1. **stop-the-world**：暂停所有非 SLO 修复的 release
2. 全员转 SRE 模式：bug bash + 弹性增强
3. budget 恢复 50%（等下一月度）才解禁 feature 发布

例子：
- 30 天 budget = 21.6min downtime (99.95%)
- 月初挂了 30min → budget 已超 38%
- 接下来一个月只批 P0 修复 + post-mortem

---

## 监控仪表板（必看 5 个）

每个 oncall 班次开始 15min 看一遍：

1. **`payments-platform-overview`**
   - 各服务 up{} 状态
   - 各服务 P99 latency
   - charge_total / refund_total / authorize_total trend
   - circuit state 矩阵

2. **`funds-safety-dashboard`**
   - outbox lag (per service)
   - reconcile pending count
   - HARD decline rate (per network)
   - audit chain verify failures (target = 0)

3. **`db-health-dashboard`**
   - pool open / in_use / idle / wait_count (per shard)
   - slow query log count
   - replication lag (if applicable)

4. **`network-availability-dashboard`**
   - 5 个卡组织各自 P99 / error rate
   - 各 adapter circuit transitions
   - reconcile worker last_corrected count

5. **`saturation-dashboard`**
   - card-payment bulkhead utilization
   - goroutine count per service
   - Go GC pause P99
   - TCP connections

---

## SLI 对应 Prometheus query

| SLI | PromQL |
| --- | --- |
| api-gateway 可用性 | `sum(rate(http_requests_total{status=~"2..\|3.."}[5m])) / sum(rate(http_requests_total[5m]))` |
| order-core charge P99 | `histogram_quantile(0.99, sum(rate(paycore_charge_duration_seconds_bucket[5m])) by (le))` |
| accounting outbox lag | `acct_outbox_lag_seconds` |
| card-payment HARD rate | `sum(rate(paycard_decline_category_total{category="HARD"}[5m])) / sum(rate(paycard_authorize_total[5m]))` |
| circuit open rate | `count(paycard_circuit_state > 0) / count(paycard_circuit_state)` |
| bulkhead saturation | `paycard_bulkhead_active / paycard_bulkhead_capacity` |

---

## 改动这个文档时

任何 SLO 调整都要走 SRE review；不能因为达不到就放宽。要么改实现，要么承认现状（写 acknowledgement）。
