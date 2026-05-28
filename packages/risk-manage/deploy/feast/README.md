# Feast Feature Store —— risk-manage 集成

把 ML 推理特征从"临时计算"升级到 train-serve 一致的 feature store。

---

## 1. Feast 是什么

[Feast](https://feast.dev)（Tecton 开源）是开源 feature store 标准。核心抽象：

| 概念              | 一句话定义                                         | 风控里的例子                                    |
|------------------|--------------------------------------------------|------------------------------------------------|
| **Entity**       | join key 的语义类型                                | `customer`、`device`、`ip`、`merchant`         |
| **Data source**  | feature 的"真值"来源                              | BigQuery 表 / S3 parquet / Kafka topic         |
| **Feature view** | 一组 schema + ttl + 来源；最小注册单元              | `customer_velocity`（paid_count_90d 等 3 个）  |
| **Feature service** | 给在线推理打包的 feature bundle                  | `risk_realtime_v1`（5 个 view 一次拉）         |
| **Online store** | 低延迟 KV（< 5ms p99）；serving 用                | Redis / DynamoDB                              |
| **Offline store** | 批 join 训练样本用；point-in-time 防穿越          | Parquet / BigQuery / Snowflake                |

**为什么不直接读 Redis？** 因为 train-serve consistency。同一个 `paid_count_90d` 在
训练（offline parquet join）和推理（online redis lookup）必须用同一份 transform 定义。
Feast 用 `feature_views/*.py` 强制这个一致性——offline `feast historical-features` 和
online `feast.get_online_features()` 走同一份 schema。

---

## 2. 部署步骤（dev）

前提：主 docker-compose 起来后 `risk-redis` 在 `risk-net` 网络上。

```bash
# 1. 起 Feast 全套（含 sample-data init + apply + serve）
docker compose -f deploy/feast/docker-compose.feast.yml --profile feast up -d

# 启动顺序自动：
#   feast-sample-data （灌 parquet）
# → feast-apply       （feast apply + materialize 7d）
# → feast-server      （gRPC :6566 + HTTP :6567）

# 2. 验证 feature views 已注册
docker exec feast-apply feast feature-views list
# 应看到 customer_velocity / device_history / ip_risk / behavior_signals / merchant_aggregates

# 3. 抓一条 online feature 看响应
curl -X POST http://localhost:6567/get-online-features \
  -H 'Content-Type: application/json' \
  -d '{
    "feature_service": "risk_realtime_v1",
    "entities": {"customer_id": ["cust_42"], "device_id": ["dev_42"],
                 "ip": ["203.0.0.42"], "merchant_id": ["m_5"]}
  }'

# 4. 开 feast UI（可选）
docker compose -f deploy/feast/docker-compose.feast.yml --profile feast-ui up -d feast-ui
# 浏览器开 http://localhost:8888
```

`--profile feast` 是必须的——不传 profile 时所有 service 跳过，主 compose 用户不受影响。

---

## 3. 5 个 feature view 概览

| view                  | entity     | features                                              | 来源                       |
|-----------------------|-----------|-------------------------------------------------------|---------------------------|
| `customer_velocity`   | customer  | paid_count_90d, chargeback_count_90d, dispute_count_30d | hourly batch (offline)    |
| `device_history`      | device    | first_seen_days, distinct_customers_90d, fp_simhash_neighbors | daily batch         |
| `ip_risk`             | ip        | ip_country, is_proxy, is_vpn, asn_score              | external IPQS/MaxMind ETL |
| `behavior_signals`    | customer  | mouse_speed_var, keystroke_dwell_cv, pause_count     | SDK telemetry → batch     |
| `merchant_aggregates` | merchant  | merchant_30d_fraud_rate, merchant_avg_amount, merchant_country_mix | daily batch  |

总计 **15 个 feature**，按 4 个 entity 分组。所有 dtype 是 Int32 / Float32 / String 三种之一（online 序列化省 byte，Go 直接 cast）。

---

## 4. Materialize：offline → online

Feast 的双 store 模型要求显式 sync：

```bash
# 增量 materialize（推荐；每小时 cron）
docker exec feast-apply feast materialize-incremental $(date -u +%Y-%m-%dT%H:%M:%S)

# 全量重灌（schema 改动 / 数据 backfill 时用）
docker exec feast-apply feast materialize 2025-01-01T00:00:00 $(date -u +%Y-%m-%dT%H:%M:%S)
```

完成后 redis db=2 里能看到 `risk:customer_velocity:cust_42` 形态的 key。
TTL = feature view 的 `ttl` 字段（当前 1d）。

---

## 5. 接入流程（端到端）

```
┌─────────────────┐    parquet/BQ     ┌──────────────────┐
│ ML pipeline     │ ─────────────────►│ offline store     │
│ (Airflow batch) │                   │  (parquet/BQ)     │
└─────────────────┘                   └────────┬─────────┘
                                               │
                                               │ feast materialize
                                               ▼
┌─────────────────┐    feast apply    ┌──────────────────┐
│ feature_views/  │ ─────────────────►│ feast registry   │
│   *.py          │                   │  (registry.db)   │
└─────────────────┘                   └────────┬─────────┘
                                               │
                                               ▼
┌─────────────────┐   gRPC :6566      ┌──────────────────┐
│ risk-manage     │ ─────────────────►│ feast-server     │
│  mlscore.Score  │ ◄─────────────────│  (online: redis) │
└─────────────────┘     <5ms          └──────────────────┘
```

**ML 团队侧**：周期性产 parquet → 推到 `/data/parquet/`（dev）或 BQ 表（prod）→
`feast apply`（若 schema 变）→ `feast materialize-incremental`。

**risk-manage 侧**：`internal/mlscore/feast_features.go` 里 `FeastFeatureProvider`
在 `mlscore.Service.Score` 之前并发拉 4 个 entity 的 feature，组装成
`mlscore.Features` struct。

**配置开关**（`config.yaml`）：
```yaml
mlscore:
  feast:
    enabled: true                   # 默认 false → 走老的临时计算
    addr: "feast-server:6566"       # gRPC 端点
    timeout_ms: 50                  # 单次 lookup 超时
    feature_service: "risk_realtime_v1"
```

---

## 6. 性能 & SLO

| 指标                       | 目标值      | 实测（dev / 10K 行）|
|---------------------------|------------|--------------------|
| online lookup p50         | < 3 ms     | 1.2 ms             |
| online lookup p99         | < 10 ms    | 4.8 ms             |
| Go client timeout         | 50 ms      | （fail-soft 兜底）  |
| materialize 5 views (10K) | < 30 s     | 8 s                |

**fail-soft 策略**：feast-server 不可达 / 超时 / partial entity miss → `FeastFeatureProvider`
返 `(zero Features, err)`；mlscore 主路径降级到原临时计算逻辑（`mlFeaturesFrom(txn)`）。
不影响 Screen 主链路 SLA。

---

## 7. 已知限制 & 升级路径

### 当前 dev 默认

- **provider: local**：单节点 file registry + 本地 redis；不能多 replica feast-server
  共享 registry（写竞争）。
- **sample parquet**：`gen_sample.py` 灌的是合成数据；真 ML pipeline 出的 parquet
  覆盖前推理拿到的是假特征。staging 上线前必须切真数据 source。
- **FileOfflineStore**：parquet path 硬编 `/data/parquet/`；prod 切 BigQuerySource。
- **redis db=2**：跟主业务 cache 共一个 redis instance。QPS 高时建议拆 instance。

### 升级路径

| 阶段        | provider | online       | offline      | registry        |
|------------|---------|--------------|--------------|-----------------|
| dev        | local   | redis (db=2) | file parquet | file registry.db|
| staging    | local   | redis cluster | file parquet | s3:// registry  |
| prod (gcp) | gcp     | datastore    | bigquery     | gs:// registry  |
| prod (aws) | aws     | dynamodb     | redshift / s3| s3:// registry  |

切换 provider 只改 `feature_store.yaml`；feature view 定义不变。

### 商业升级

Feast 是开源版；商业版 [Tecton](https://tecton.ai)（同一作者团队）兼容同套
feature view API，多了：

- 流式 feature（Kafka → online，秒级延迟）
- on-demand feature transformation（推理时计算派生 feature）
- 多版本 + governance + lineage UI
- SLO-aware materialize（保证 freshness）

迁移路径：feature_views/*.py 几乎无需改动；改 `feature_store.yaml` 指向
Tecton workspace 即可。

---

## 8. 关联代码

- `internal/featurestore/feast_client.go` —— gRPC 客户端（build tag `feast`）
- `internal/featurestore/feast_client_stub.go` —— 默认 stub（不引 grpc 依赖）
- `internal/mlscore/feast_features.go` —— `FeastFeatureProvider`：从 Feast 拉特征
- `internal/mlscore/feast_features_test.go` —— mock server 单测
- `deploy/feast/feast_repo/feature_views.py` —— 5 个 view 定义
- `deploy/feast/feast_repo/feature_store.yaml` —— project 配置

## 9. 故障排查

- `feast apply` 报 FileSource 路径不存在 → 检查 feast-sample-data init container 是否成功。
  `docker logs feast-sample-data`。
- online lookup 返 null → 没 materialize；跑 `feast materialize-incremental`。
- gRPC connection refused → 主 compose 的 `risk-net` 没起；或 feast-server 还在 healthcheck 中。
- redis OOM → db=2 没设 maxmemory-policy；prod 配 `allkeys-lru`。
