# Commercialization Guide — risk-manage

把 risk-manage 做成"对外可销售 / 多商户多区域 / 7×24 SLA"的实时风控系统所需的全部能力对照。

## 1. 架构总览（落地后 vs 默认）

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                       Frontend SDK  (Web JS / iOS / Android)                 │
│                  ─→  /v1/risk/session  ─→  RiskSessionID                     │
└──────────────────────────────────────────────────────────────────────────────┘
                                        │
                                        ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                  API Gateway  (Auth / RateLimit / mTLS)                      │
└──────────────────────────────────────────────────────────────────────────────┘
                                        │
                                        ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│ risk-manage  (Go, this repo)                                                 │
│                                                                              │
│   Fingerprint Engine  ─┐                                                     │
│   Behavior Engine     ─┼─→ Feature Aggregator (TxnContext)                   │
│   IP Intelligence     ─┤      │                                              │
│   Session Store       ─┘      ▼                                              │
│                          ┌────────────┐    ┌────────────┐                    │
│                          │ Rule Engine│ ←→ │ ML Scoring │   <- gRPC          │
│                          └────────────┘    └────────────┘                    │
│                                  │                                            │
│                                  ▼                                            │
│                          Decision Engine (score → verdict, per-merchant)     │
│                                  │                                            │
│                  ┌───────────────┼─────────────────────────┐                 │
│                  ▼               ▼                         ▼                 │
│              Audit Sink     Review Queue             Feedback Recorder       │
│                  │               │                         │                 │
└──────────────────┼───────────────┼─────────────────────────┼─────────────────┘
                   │               │                         │
                   ▼               ▼                         ▼
            ClickHouse         Postgres                 Postgres
            (analytics)      (admin UI)                (ML training)
                   │
                   └─→ Kafka topic for fan-out

Storage: Redis (real-time counters / sessions / blacklist), Neo4j (link graph)
```

## 2. Feature 覆盖映射

| 用户需求       | 模块 / 规则                                           | 状态 |
| ------------ | ---------------------------------------------------- | ---- |
| 设备复用识别  | `link_fanout` 规则 + `store.LinkStore`               | ✅ |
| 多账号关联    | 同上（device → customer 边）                          | ✅ |
| Bot 检测     | `bot_detection`（设备 + 行为复合）+ `behavior_anomaly` | ✅ |
| VPN/proxy 检测 | `ip_risk` + `ipintel.Service`                      | ✅ |
| 风险评分      | engine score matrix + thresholds + per-merchant override | ✅ |
| 反馈闭环      | `review` + `feedback` 包                              | ✅ |
| ML 集成      | `mlscore.Service` + `ml_threshold` 规则               | ✅ |
| **商户级 API key + tenant 隔离** | `auth` 包（Phase 13a）           | ✅ |
| **3DS step-up 触发**           | payment-core REVIEW → requires_action(three_d_secure) | ✅ |
| **商户 allow / block list**    | `merchantlist` + `merchant_allowlist` / `merchant_blocklist` | ✅ |
| **Outbound webhook**          | `webhook` 包，HMAC-SHA256 签名 + 重试    | ✅ |
| **规则回测 (backtest)**        | `cmd/replay --candidate-rule --candidate-thresholds` | ✅ |
| **多跳图欺诈环检测**           | `link_fanout_multihop` + `LinkStore.PeersWithin` | ✅ |
| **图风险传播 (tag propagation)** | `graph_reputation` + `Tag` / `TagsWithin` | ✅ |
| **跨商户欺诈共享检测**         | `cross_merchant_link` | ✅ |
| **卡测试检测**                 | `card_testing` | ✅ |
| **下游熔断器 (ipintel/mlscore)** | `reliability.Breaker` | ✅ |
| **per-merchant Screen QPS 限流** | `reliability.MerchantLimiter` (token bucket) | ✅ |
| **审计链式签名 (tamper-evident)** | `audit.ChainSink` (sha256 链 + VerifyChain) | ✅ |
| **AML / 制裁名单筛查**         | `sanction.Service` + `sanction_screening` 规则 | ✅ |
| **沙箱 + 测试卡号**            | `sandbox.Detect` (rsk_test_* key + risk_test_* triggers) | ✅ |
| **运营自助规则 DSL**           | `dsl` 规则类型（fact-based 条件，AND/OR + 12 种 op + metadata.* 访问） | ✅ |
| **Impossible travel 检测**     | `impossible_travel`（speed_kmh 阈值 + window_min） | ✅ |
| **老客户信任分**               | `returning_customer`（90d paid count + chargeback count → Force-Allow） | ✅ |
| **AVS 地址验证**               | `avs_check`（N=Force-Deny, A/Z=Review, Y/X=skip） | ✅ |
| **BIN 国家一致性**             | `bin_country`（卡 issuer 国 vs 商户声明国，跨境加分） | ✅ |
| **Email validation**           | `email_validation`（disposable + email_age_days） | ✅ |
| **滑窗金额累计速率**           | `velocity_amount` + Counter.GetVelocityAmount 接口 | ✅ |
| **规则灰度 / Staged rollout**  | `engine.RolloutConfig`（bucket_field + enable_pct，sha256 稳定分桶） | ✅ |
| **OpenAPI / Swagger 文档**     | `api/openapi.yaml`（覆盖 SDK / admin / webhook contract） | ✅ |
| **真实 ML 模型（默认开箱）**   | `mlscore.LogisticService` 13 特征先验校准 LR；接口可换 XGBoost/TF-Serving | ✅ |
| **ML 模型漂移监控**            | `mlscore.DriftMonitor` 滑窗 + baseline 比较；admin 端点查 / setBaseline | ✅ |
| **Node 服务端 SDK（运维用）**  | `clients/node/` 含 webhook 验签 + admin endpoints wrapper | ✅ |

## 3. 商业部署清单（required）

### 3.1 持久化（替换 default mem 实现）

| 数据             | 接口              | 默认       | 生产        | 文件                                  |
| --------------- | ----------------- | --------- | ---------- | ------------------------------------ |
| 限额计数器       | `store.Counter`   | MemCounter | Redis      | `internal/store/redis_counter.go`    |
| 黑名单           | `store.Blacklist` | MemBlacklist | Redis SET / PG | -（按 redis_counter 范式扩展） |
| 设备 / IP / Customer 图谱 | `store.LinkStore` | MemLinkStore | Neo4j  | `internal/store/neo4j_linkstore.go`  |
| Web SDK session | `session.Store`   | MemStore   | Redis HSET | -                                    |
| Review queue   | `review.Store`    | MemStore   | Postgres   | `internal/store/postgres_review.go`  |
| Outcome feedback | `feedback.Recorder` | MemRecorder | Postgres | 同上文件                            |
| 决策审计       | `audit.Sink`      | LogSink+MemSink | Kafka → ClickHouse | `internal/audit/clickhouse_sink.go` |

build tag 启用：

```bash
go build -tags 'pg neo4j clickhouse redis' ./cmd/server
```

### 3.2 多商户能力

`engine.PolicyStore` + `engine.MerchantPolicy`：

```yaml
# admin POST /admin/merchants/<id>/policy
review_min: 15            # 高风险行业更严
deny_min:   45
disabled_rules: ["country_block"]    # 商户已自行做地理限制
weight_overrides:
  ip_risk: 60             # 加权某条规则
```

无 override 的商户走全局 baseline（`engine.SetScoreThresholds`），完全向后兼容。

PG 实现 schema：

```sql
CREATE TABLE risk_merchant_policy (
    merchant_id    TEXT PRIMARY KEY,
    review_min     INT,
    deny_min       INT,
    disabled_rules JSONB,
    weight_overrides JSONB,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by     TEXT
);
```

### 3.3 鉴权

公网 + admin 端点分离：

- 公网（SDK 直接调）：`/v1/risk/session`、`/healthz`、`/metrics`
- admin 内网（admin-web / 服务账号）：`/admin/*`

`metrics.AdminAuth(tokens)` middleware 在 `Authorization: Bearer <token>` 上做常量时间比对：

```go
adminAuth := metrics.AdminAuth(tokensFromConfig)
adminMux  := http.NewServeMux()
review.RegisterHandlers(adminMux, store, logger)
feedback.RegisterHandlers(adminMux, recorder, logger)
parentMux.Handle("/admin/", adminAuth(adminMux))
```

生产搭配：

- mTLS 在 ingress / api-gateway 层做服务身份
- token 走 KMS / Vault 拉取，30d 轮换
- admin-web 是真正人员入口，由它转发到此服务（带服务账号 token）

### 3.4 SLO / 可观测

| 指标                           | 用途                                       |
| ----------------------------- | ----------------------------------------- |
| `risk_screen_total{decision}` | 全局 verdict QPS                          |
| `risk_verdict_total{merchant_id, verdict}` | **商业核心**：按商户的 approval / review / deny 比例 |
| `risk_score{merchant_id}`     | 风险分分布；阈值附近聚集 → 阈值需调            |
| `risk_screen_duration_seconds`| Screen p99 延迟（SLA 一般 ≤ 50ms）          |
| `risk_rule_evaluation_total{rule_id, hit}` | 单条规则命中率，找误杀 / 抓漏      |
| `risk_review_queue_depth`     | 运营值班负载，> 1000 触发告警               |
| `risk_outcome_total{source, is_fraud}` | 反馈量 → 算 ML precision / recall      |

预设告警：

```yaml
# Prometheus alerting rules
- alert: RiskScreenLatencyHigh
  expr: histogram_quantile(0.99, rate(risk_screen_duration_seconds_bucket[5m])) > 0.05
  for: 5m
- alert: RiskReviewQueueOverflow
  expr: risk_review_queue_depth > 1000
  for: 10m
- alert: RiskRuleErrorRate
  expr: rate(risk_rule_evaluation_total{hit="error"}[5m]) > 0.01
  for: 5m
- alert: RiskMerchantDenyRateAbnormal
  expr: |
    rate(risk_verdict_total{verdict="DENY"}[1h])
      / rate(risk_verdict_total[1h]) > 0.20
  for: 30m
```

### 3.5 幂等性

`TxnContext.IdempotencyKey` 非空 → 60s 内同 key 复用上次 Result，避免：

- payment-core 重试导致 audit / review queue 重复
- 多数据中心切流的 double-screen
- ML 推理重复调（贵）

业务侧推荐传 `payment_intent_id + retry_idx` 或商户 `client_token`。

## 4. 部署形态



### 4.1 单 Region（小客户）

```
┌────────────┐    ┌────────────┐    ┌────────────┐
│ risk-manage│ ─→ │   Redis    │    │ Postgres   │
│  (3 pods)  │    │  (cluster) │    │ (primary)  │
└────────────┘    └────────────┘    └────────────┘
       │
       └────────────→ ClickHouse (single)
```

资源：3× (2 vCPU, 4 GB RAM)，Redis 4 GB, PG 8 GB, CH 16 GB。
SLA：p99 < 50ms / decisions/s ≤ 500。

### 4.2 多 Region（企业 / 跨境）

每 region 独立完整栈；图谱 / outcome 通过 Kafka MirrorMaker 跨 region 同步。
关键决策（Screen）走本地 region，避免跨洋 RTT。

### 4.3 灰度 / 蓝绿发布

- shadow rule mode（已实现）：新规则上线 1 周观察 `risk_rule_evaluation_total{hit="shadow_hit"}` ⇒ enforce
- 模型升级：`mlscore.Service` 的 ModelVer 落 audit；A/B 用 IPHash 路由 30% 流量
- 阈值调整：`SetScoreThresholds` 热生效，admin UI 改动后 5s 内全部 pod 拉到新值

## 5. 未做（按业务优先级）

- [x] **PG-backed `engine.PolicyStore`** — `internal/store/postgres_policy.go`（//go:build pg），atomic-snapshot cache + 30s 后台 reload + admin invalidate
- [x] **Admin token hot-reload** — `metrics.NewFileTokenSource(path, interval, logger)` + `AdminAuthFromSource`；KMS 集成只需新写一个 `TokenSource` 实现连 SDK
- [x] **Decision replay CLI** — `cmd/replay`，支持 stdin / 文件 / admin URL；输出 CSV 对比新旧 verdict + score diff
- [x] **PCI-DSS audit log 加密** — `audit.EncryptSink` AES-256-GCM envelope（AAD=decision_id 防 ciphertext swap），key rotation by `KeyProvider.ActiveKeyID()`
- [ ] **Bandit / RL 自动阈值优化**（需要离线训练 pipeline，不是单服务能闭环）
- [ ] **Feature store 离线 / 在线一致性**（Tecton / Feast 集成 — 需要外部基础设施选型）

## 6. 加密 / token / replay 用法速查（接 § 5）

```bash
# 加密 audit log（envelope 模式）
export ACTIVE_KEY_ID=v1
export RISK_AUDIT_KEY_v1=$(openssl rand -hex 32)
# main.go newAuditSink 里把 EncryptSink 包到 ClickHouse / Kafka sink 外面

# 文件托管的 admin token（每行一条；# 注释；空行忽略）
cat > /run/secrets/risk_admin_tokens <<EOF
# admin-web service account
$(openssl rand -hex 32)
# dispute system
$(openssl rand -hex 32)
EOF
# config.yaml:
#   admin:
#     tokens_file: /run/secrets/risk_admin_tokens
#     tokens_reload: 30s

# 决策 replay：拿当前规则集对历史 1000 条决策做回归
risk-replay --config config/config.yaml \
            --url http://risk:9590/admin/audit/decisions?limit=1000 \
            --token "$ADMIN_TOKEN" > replay.csv
awk -F, '$7=="false"' replay.csv | head    # 找规则改动后 verdict 翻车的 case
```

## 7. 计费 & SLA

商业版按 `risk_screen_total` 计费；SLA 99.95%（fail-open，超时算成功调用且 verdict=ALLOW）。

| 套餐    | QPS   | 商户数 | SLA    | 价格            |
| ------ | ----- | ----- | ------ | -------------- |
| Starter | 50    | ≤ 5  | 99.9%  | $X / mo + per-call |
| Pro    | 500   | ≤ 50  | 99.95% | $Y / mo + per-call |
| Enterprise | 5000+ | ∞    | 99.99% | 合同议价         |
