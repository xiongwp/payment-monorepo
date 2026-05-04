# risk-manage Reliability & Chaos Test Runbook

## SLO

| Tier | RTO | RPO | 说明 |
|---|---|---|---|
| risk-manage 自身 | 60s | 0 | 任意副本可换；fail_open 兜底 |
| LinkStore (Redis) | 5min | 1h 数据丢失上限 | warm-start 从 audit 回放 |
| Counter (Redis) | 5min | 自然 TTL 修复 | 重启后 daily/monthly 临时归零 |
| review queue (PG) | 60s | 0 | PG 主备 / sync replication |
| audit sink | 0 | 0 | 链式签名 + 异步多副本写入 |

## 常见故障 + 应对

### 1. Redis 重启 / failover

**症状**：LinkStore-based 规则（register_velocity / fingerprint_multi_account /
link_fanout / multi_hop_ring）瞬间空数据 → 攻击者 1-5 分钟无防御窗口。

**自动应对**：
- `startWarmup` fx hook 启动 5s 后从 audit MemSink 拉最近 1h 决策回放进 LinkStore
- `warmup.lookback_hours` 配置可调
- `warmup.disabled=true` 关

**人工应对**：
```bash
# 强制触发一次 warmup（重启服务）
docker compose restart risk-manage

# 看 warmup log（用 compose service 名，跨多副本聚合）
docker compose -p risk-manage logs risk-manage 2>&1 | grep "linkstore warmup"
# 期望：linkstore warmup complete audit_rows=N edges_written=M
```

**预防**：
- Redis 配 AOF + appendfsync everysec（最坏丢 1s）
- Sentinel / Cluster 至少 3 节点
- 升级前先把 audit ClickHouse sink 拉满（warm-start 数据源）

### 2. risk-manage 进程重启

**症状**：
- `IntervalTracker` 进程内 → register_interval 首笔不能比对（30s 内能拼 N 笔）
- `idempotencyCache` 进程内 → 60s 幂等保证瞬间失效
- `otpChallenges`（user-merchant-core）→ 用户填一半 OTP 失效

**自动应对**：
- gRPC `GracefulStop` + fx StopTimeout 30s drain in-flight RPC
- `/readyz` 立刻 503 让 LB 摘流量
- K8s rolling update 滚一台等一台

**人工应对**：
```bash
kubectl rollout status deploy/risk-manage --timeout=2m
```

**预防**：
- PodDisruptionBudget 保证至少 N-1 副本
- HPA 扩容到 ≥3 副本
- LinkStore 用 Redis（cross-process 共享）—— 重启不丢

### 3. review queue 持久化失败

**症状**：MemStore 重启 → 全部 pending case 丢失，运营投诉"我刚审到一半"。

**应对**：
- 切到 `PGReviewStore`（build tag `pg`）
- Schema 见 `internal/store/postgres_review.go` 顶部注释

```bash
# 启动时确认
docker compose -p risk-manage logs risk-manage 2>&1 | grep "review store"
# 期望：review store: postgres dsn=...
```

**演练**：
```bash
# 1. 灌一批 pending case
for i in {1..20}; do
  curl -X POST .../api/risk/screen ...
done

# 2. 重启服务
docker compose restart risk-manage

# 3. 确认 case 还在
curl .../api/risk/reviews/list?status=pending | jq '.[] | .id'
```

### 4. audit chain 断链

**症状**：合规审计时发现 chain_prev_hash → chain_row_hash 链不连续。

**应对**：
- 配置 `audit.chain_signing=true` + 持久化 sink（ClickHouse / PG）
- 启动时从持久 sink SELECT 最后一行 row_hash → `audit.NewChainSinkResume`
  （主路径 wrapper 会把这个 hash 当 prev 续写）

**预防**：
- 不要在 mem-only 模式下开 chain_signing（重启即从 genesis 开始 → 假断链）

### 5. 单条规则失效

**症状**：某条 deny 规则 7 天 0 hit，可能：
- yaml 字段拼错（默认值生效）
- caller metadata 没接入
- 阈值配的太宽

**自动应对**：
- `synthetic` worker 每分钟跑 fixture 验证关键规则触发；mismatch → Prometheus alert

**人工应对**：
```bash
# 看 rule_eval 计数
curl .../metrics | grep risk_rule_eval_total | grep r_my_rule

# 用 explain 端点 dry-run 一笔已知该命中的样本
curl -X POST .../admin/explain -d '{"decision_id":"<known_id>"}' | jq '.replayed.hits'
```

## Chaos 演练计划

### 月度 (Tier 1)
1. 杀 Redis pod，观察 risk-manage fail-open 路径，确认 5min 内 warmup 恢复
2. 滚动重启 risk-manage 全部副本，确认无 RPC 错误飙升
3. 杀 PG primary，观察 review store 切到 replica 时间

### 季度 (Tier 2)
1. 全 region failover：把 risk-manage primary region 流量切到 DR
2. audit ClickHouse 查询过载 → 看是否反压主路径
3. backup 还原演练：从昨天的 PG/ClickHouse backup 恢复，跑 1h replay 验证

### 年度
1. 全栈 RTO 演练：从零部署 risk-manage + 数据恢复，跑通 / signup / login / payment
2. 合规审计：审 30 天 audit chain 是否连续，PII masking 覆盖率

## Backup 策略

| 数据 | 频率 | 保留 | RTO |
|---|---|---|---|
| Postgres (review queue) | 每小时 PITR | 30 天 | 30min |
| ClickHouse (audit) | 每天 full + 每小时 incremental | 7 年 | 1h |
| Redis (LinkStore / Counter) | 不 backup | N/A | warm-start 从 audit |
| Rule yaml | git | 永久 | 即时 |
| ML model config | git + S3 versioned | 永久 | 即时 |

## 关键 Alert

```
# Redis 失联
alert: RiskRedisDown
expr: up{service="redis-risk"} == 0
for: 30s

# warmup 没跑完 (启动 10min 后还没看到 linkstore_edges 增长)
alert: RiskWarmupFailed
expr: rate(risk_warmup_edges_total[5m]) == 0 and time() - process_start_time_seconds < 600

# Synthetic mismatch (规则 silent regression)
alert: RiskRuleSilentRegression
expr: rate(risk_synthetic_probe_total{result="mismatch"}[5m]) > 0
for: 1m

# SLA overdue 堆积
alert: RiskReviewSLAOverdue
expr: risk_review_overdue > 50

# Score drift
alert: RiskMLDrift
expr: risk_ml_score_drift_pct > 30
for: 10m
```
