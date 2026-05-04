# risk-manage 运维 Runbook

值班手册。每条 Alert 对应 1-2 条排查步骤；新告警必须先来本文档加 entry。

## 目录

- [Tier 1 — 主路径 SLA](#tier-1--主路径-sla)
- [Tier 2 — ML / 规则](#tier-2--ml--规则)
- [Tier 3 — 反馈 / 存储](#tier-3--反馈--存储)
- [应急工具箱](#应急工具箱)

---

## Tier 1 — 主路径 SLA

### `RiskScreenLatencyHigh` (Screen p99 > 100ms)

**判**：

```bash
# 看 5-stage 延时分布，找最慢
curl -s :9590/metrics | grep risk_screen_stage_duration_seconds_sum | grep stage=
# 看慢请求按 stage 拆分
curl -s :9590/metrics | grep risk_screen_slow_total
```

**修**：

| 最慢 stage | 处理 |
|---|---|
| `feature_extract` | 拉 pprof CPU profile，看具体 extractor；可能 LinkStore 多跳查询太重 |
| `ip_intel` | 检查 ipintel 服务延时；调高 ipintel breaker 阈值 |
| `ml_score` | RemoteModelService 远程慢？检查 challenger panel 看 challenger latency；考虑 `/admin/mlscore/override` `disabled=true` 临时关 ML |
| `engine_eval` | rule 数太多？检查规则 KPI panel 找 silent / 误伤规则删 |
| `audit_write` | sync sink 阻塞？应该已经 AsyncBatchSink；查 `risk_audit_async_dropped_total` |

### `RiskScreenErrorRate` (Error rate > 1%)

```bash
# 看错误码分布
curl -s :9590/metrics | grep risk_grpc_request_total
# 拉最近 audit 看 reasons
curl -s :9590/admin/audit/decisions?limit=50 -H "Authorization: Bearer $ADMIN_TOKEN"
```

**常见原因**：上游 payment-core 传非法 payload / merchant_id 不匹配 / API key 被撤。

---

## Tier 2 — ML / 规则

### `MLScoreDrifted` (ML 分数漂移)

**判**：

```bash
curl -s :9590/admin/mlscore/drift -H "Authorization: Bearer $ADMIN_TOKEN" | jq .
```

**修**：

1. 看 `drifted=true` 的 metric (mean / p95 / drift_pct)；
2. 如果是 **新模型刚上线**，调 `POST /admin/mlscore/drift/baseline` 重设基线；
3. 如果不是新上线 → 数据分布真变了：
   - 短期：`POST /admin/mlscore/override force_score=0` 让规则兜底
   - 中期：跑 `cmd/retrain` 重训模型 + Platt 校准 + 注册新 challenger
   - A/B：在 `/risk/abtest` 页观察新 challenger 显著优于 champion 后 promote

### `RuleSilent7d` (规则 7 天没命中)

**判**：

```bash
curl -s :9590/admin/rules/insights -H "Authorization: Bearer $ADMIN_TOKEN" | jq '.rules[] | select(.is_silent)'
```

**修**：可能 1) 规则配错（type 写错 / config_json schema 不对）；2) 数据格式变了（IP intel 返回字段重命名）；3) 规则过期。

```bash
# 看规则定义 + 测试一笔历史决策
curl -s :9590/admin/rules/list -H "Authorization: Bearer $ADMIN_TOKEN" | jq '.[] | select(.id=="r_xxx")'

# 用 simulator 跑历史样本看是否命中
curl -X POST :9590/admin/rules/simulate -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{...candidate rule...}'
```

### `RuleLowPrecision` (precision < 30%)

误伤规则。`/risk/rules` 页找该规则 → 改 mode 到 shadow → 跑 N 天观察 → 决策。

---

## Tier 3 — 反馈 / 存储

### `OutcomeCoverageLow` (DENY coverage < 50% 持续 1h)

**判**：

```bash
curl -s :9590/admin/dashboard/recall -H "Authorization: Bearer $ADMIN_TOKEN"
```

**常见原因**：

- order-core dispute hook 没在 dispute 终态调 `POST /admin/feedback/dispute`
- merchant_confirm 端点 down
- review queue 决策没 fanout 到 feedback recorder

```bash
# 直接调 dispute 端点测
curl -X POST :9590/admin/feedback/dispute \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"payment_intent_id":"pi_test","is_fraud":true,"actor":"manual_test"}'
```

### `WebhookDLQDepthGrowing` (DLQ > 100 + 增长)

**判**：

```bash
# 看 DLQ 列表
curl -s ":9590/admin/webhook/dlq?limit=100" -H "Authorization: Bearer $ADMIN_TOKEN" | jq

# 找哪个商户问题最大
curl -s ":9590/admin/webhook/dlq?limit=500" -H "Authorization: Bearer $ADMIN_TOKEN" | \
  jq -r '.items[].merchant_id' | sort | uniq -c | sort -rn | head
```

**修**：

```bash
# fix 商户 webhook URL 后批量 replay 该商户的 DLQ
for id in $(curl -s ":9590/admin/webhook/dlq?merchant_id=$BAD_MERCHANT&limit=200" \
              -H "Authorization: Bearer $ADMIN_TOKEN" | jq -r '.items[].event_id'); do
  curl -X POST :9590/admin/webhook/dlq/replay \
    -H "Authorization: Bearer $ADMIN_TOKEN" -d "{\"event_id\":\"$id\"}"
done
```

不可恢复 → `POST /admin/webhook/dlq/discard {event_id, reason}`。

### `AuditAsyncSinkBackpressure` (audit dropped)

inner sink (FileSink / KafkaSink / ClickHouseSink) 跟不上。

```bash
# 看 queue 深度
curl -s :9590/metrics | grep risk_audit_async
```

**修**：

- 增加 `audit.async.queue_size` / `audit.async.batch_size`
- 加 inner sink 实例数 (Kafka producer 多 partition / CH 多副本)
- 短期：临时禁用 chain_signing (它增加每条 audit ~100µs)

---

## 应急工具箱

### 紧急关 ML

```bash
curl -X POST :9590/admin/mlscore/override \
  -H "Authorization: Bearer $ADMIN_TOKEN_DANGER" \
  -d '{"disabled":true,"reason":"drift_alert_2026_04_30"}'
# 恢复
curl -X POST :9590/admin/mlscore/override/clear \
  -H "Authorization: Bearer $ADMIN_TOKEN_DANGER"
```

### 紧急关单条规则

```bash
# 切到 shadow （命中只统计，不影响 verdict）
curl -X POST :9590/admin/rules/mode \
  -H "Authorization: Bearer $ADMIN_TOKEN_DANGER" \
  -d '{"id":"r_xxx","shadow":true}'
```

### CPU profile / heap dump

```bash
# 30s CPU profile
curl -o cpu.prof "http://:9590/admin/debug/pprof/profile?seconds=30" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
go tool pprof -http=:8080 cpu.prof

# heap snapshot
curl -o heap.prof "http://:9590/admin/debug/pprof/heap" \
  -H "Authorization: Bearer $ADMIN_TOKEN"

# goroutine dump
curl -o goroutine.txt "http://:9590/admin/debug/pprof/goroutine?debug=2" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

### 验证审计链完整性

```bash
curl -X POST :9590/admin/audit/chain/verify \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"limit":1000}' | jq
```

### 跑混沌测试

```bash
# 见 cmd/chaos/chaos.sh
./cmd/chaos/chaos.sh redis-restart
./cmd/chaos/chaos.sh risk-restart
./cmd/chaos/chaos.sh review-restart
./cmd/chaos/chaos.sh probe
```

---

## 联系人

- L1 oncall：see PagerDuty rotation `risk-manage-primary`
- L2 escalation：`payment-platform-leads` Slack channel
- 风控产品 owner：see /docs/owners.md
