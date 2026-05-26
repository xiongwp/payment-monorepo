# Feast Online Feature Store Integration

## Why Feast

风控引擎现状：特征在 `mlFeaturesFrom(txn)` 里临时从 `TxnContext` 拼出来；
velocity 类（`paid_count_90d`、`chargeback_count_90d` 等）走 `internal/features/customer_history.go`
本地聚合。问题：

1. **train-serve skew**：训练侧 DS 用 SQL/Spark 在 warehouse 做特征，serving
   走 Go 代码——同一个特征两份实现，定义漂移就有 silent bug。
2. **point-in-time correctness**：训练样本需要"那一刻"的特征值；现状靠手写
   join 容易引入未来信息穿越。
3. **特征复用难**：别的服务（增长、风控反洗钱）也想用，没注册中心。

Feast (Tecton 开源版) 解法：feature view YAML 定义一次，online (Redis) +
offline (Parquet) 双写，serving 和 training 都从 Feast 读。

## 部署架构

```
DS 笔记本 ──feast apply──> Registry (Postgres)
                              │
                  ┌───────────┴───────────┐
                  │                       │
              Materialize                 │
              (Spark job)                 │
                  │                       │
            ┌─────┴─────┐                 │
            ▼           ▼                 ▼
        Online      Offline           Serving Pod
        (Redis)     (Parquet/S3)      (gRPC :6566)
            ▲                              ▲
            │                              │
       risk-manage ─────GetOnlineFeatures──┘
```

- **Registry**：feature view 元数据，DS 团队 `feast apply` 推
- **Online store**：复用已有 Redis cluster（namespace `feast:risk`）
- **Offline store**：Parquet on S3，retraining worker 离线 join
- **Serving**：独立 deployment，gRPC :6566，QPS 上限按风控 RPS 1.5x 算容量

## Feature View 定义示例

`feature_repo/customer_velocity.py` (DS 团队维护)：

```yaml
entities:
  - name: customer
    join_keys: [customer_id]

feature_views:
  - name: customer_velocity
    entities: [customer]
    schema:
      - name: paid_count_90d
        dtype: Int64
      - name: chargeback_count_90d
        dtype: Int64
      - name: avg_amount_30d
        dtype: Float32
      - name: distinct_ip_7d
        dtype: Int64
    online: true
    ttl: 7776000s  # 90d
    source: file_source/customer_velocity.parquet

feature_services:
  - name: risk_realtime_v1
    features:
      - customer_velocity:paid_count_90d
      - customer_velocity:chargeback_count_90d
      - customer_velocity:avg_amount_30d
      - customer_velocity:distinct_ip_7d
```

`feature_service` 命名约定：`{domain}_{usecase}_v{n}`。bump v 用于
breaking schema change，老 service 保留一段过渡期。

## 客户端

`internal/featurestore/feast_client.go`（build tag `feast`）：

```go
c, _ := featurestore.NewFeastClient("feast-online:6566", "risk", 50*time.Millisecond)
defer c.Close()
feats, err := c.GetOnlineFeatures(ctx, customerID, []string{
    "customer_velocity:paid_count_90d",
    "customer_velocity:chargeback_count_90d",
})
```

默认 build（无 `-tags=feast`）走 `feast_client_stub.go`，所有方法返
`ErrFeastNotEnabled`。生产镜像需要 `go build -tags=feast ./...`。

## Admin Endpoint (待实现 Phase 3)

```
POST /admin/featurestore/feast/enable
Body: {"addr": "feast-online:6566", "project": "risk", "timeout_ms": 50}
```

动态切换；切换前会 health-ping 一次，失败保留原 client。

## Server Config (cmd/server/main.go 后续注入)

```yaml
featurestore:
  feast:
    enabled: false              # 默认 off, fail-open 走 mlFeaturesFrom
    addr: feast-online:6566
    project: risk
    feature_service: risk_realtime_v1
    timeout: 50ms
```

## Train-Serve Consistency 验证

每个 release 跑一次 consistency check：

1. retrain worker 拉一批 historical decisions（带 `feature_snapshot`）
2. 对每个 decision，用 `feast.get_historical_features(entity_df, ts)` 重算同一时刻的特征
3. 对比 snapshot 里存的值 vs feast 重算值；diff > 1e-6 报警

不通过则禁止 deploy（CI gate）。

## Fail-Open 语义

- gRPC timeout (>50ms) → 返 ErrFeastTimeout，调用方 0 填充 + log warn
- 某个 feature 不在 response 里 → 0 填充 + log warn（DS 删了特征也不挂掉服务）
- Feast serving 整个挂 → 退回 `mlFeaturesFrom(txn)` 老路径

熔断器复用现有 `s.mlBreaker`，不单独再加一个。

## go.mod 影响

当前 commit 不动 go.mod。Feast Go proto 还没 vendor——ML 团队接入
proto 后补：

```
require (
    github.com/feast-dev/feast/sdk/go v0.x.y
)
```

`google.golang.org/grpc` 已是 indirect dep，feast tag 打开后变 direct。
