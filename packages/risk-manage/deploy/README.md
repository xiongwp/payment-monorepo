# risk-manage 部署

风控系统生产部署完整指南。所有依赖系统（ClickHouse / Kafka / Redis /
NebulaGraph / Prometheus）都已 first-class 集成 — 不再 build tag stub。

## 拓扑

```
                   ┌──────────────────┐
   payment-core ──→│   risk-manage    │──→ ClickHouse  (audit OLAP, 730d TTL)
                   │   (gRPC :9490)   │──→ Kafka       (Flink + CH Kafka engine)
                   │   admin :9590    │──→ Redis       (counter / velocity)
                   └────┬───────┬─────┘──→ NebulaGraph (device 关联图)
                        │       │
                        ▼       ▼
                  Prometheus   admin-web (UI + BFF)
                       │
                       ▼
                   Grafana
```

## 快速启动 (docker-compose)

完整栈一键启动（含所有依赖）：

```bash
GITHUB_TOKEN=ghp_xxx docker compose up -d
```

启动顺序由 `depends_on: { condition: service_healthy }` 控制：
1. zookeeper / redis / nebula-metad → ready
2. kafka / nebula-storaged / clickhouse → ready
3. nebula-graphd → ready
4. risk-manage 启动

服务 ports（host）：
| 服务 | 端口 | 用途 |
|---|---|---|
| risk-manage gRPC | 9490 | payment-core 调 |
| risk-manage admin | 9590 | metrics / pprof / admin |
| Redis | 16379 | host 调 |
| Kafka | 19092 | host produce/consume |
| ClickHouse HTTP | 18123 | curl 查询 |
| ClickHouse native | 19000 | clickhouse-client |
| NebulaGraph | 19669 | nebula-console |
| Prometheus | 19091 | http://localhost:19091 |
| Grafana | 13000 | http://localhost:13000 (admin/change_me) |

初始化 NebulaGraph schema（compose 启动后跑一次）：

```bash
docker exec -it risk-nebula-graphd /bin/sh -c '
echo "CREATE SPACE IF NOT EXISTS risk_graph(partition_num=10, replica_factor=1, vid_type=FIXED_STRING(32));" | \
  nebula-console -u root -p nebula -addr 127.0.0.1 -port 9669
sleep 10
cat /tmp/schema.ngql | nebula-console -u root -p nebula -addr 127.0.0.1 -port 9669
'
```

## Kubernetes (生产)

### 直接 apply

```bash
kubectl apply -f deploy/k8s/
```

按依赖顺序：
1. `secret.example.yaml`（先 cp + 改值）→ `kubectl apply -f secret.yaml`
2. `configmap.yaml` → 改 ConfigMap 里的 broker / addr 指向你的集群服务
3. `deployment.yaml` + `service.yaml` + `hpa.yaml` + `pdb.yaml` + `networkpolicy.yaml`
4. `servicemonitor.yaml`（需要 prometheus-operator）

### Helm（推荐）

```bash
helm install risk-manage deploy/helm/risk-manage/ \
  --namespace risk \
  --create-namespace \
  --set clickhouse.addr=clickhouse:9000 \
  --set kafka.brokers=kafka:9092 \
  --set adminTokens.danger=$(openssl rand -hex 32)
```

values.yaml 全字段见 `deploy/helm/risk-manage/values.yaml`。

### 上游依赖

risk-manage 假定以下服务在同 namespace 或 cross-ns DNS 可达：

- `redis-master:6379`        — Redis cluster (Bitnami helm chart)
- `kafka-bootstrap:9092`     — Strimzi / CP-K Kafka
- `clickhouse:9000`          — Altinity / official CH operator
- `nebula-graphd:9669`       — Nebula operator
- `otel-collector:4317`      — OpenTelemetry collector

各依赖的 helm chart 不在本仓维护；指向你公司既有 platform。

## ClickHouse schema

`deploy/clickhouse/init/01-schema.sql` 自动跑（compose 容器启动 hook）。
k8s 部署需要手动建 db + 跑 schema：

```bash
kubectl exec -it clickhouse-0 -- clickhouse-client < deploy/clickhouse/init/01-schema.sql
```

包含：
- `risk.risk_decision_audit` MergeTree (PARTITION BY toYYYYMM, TTL 730d)
- `risk.risk_decision_kafka` Kafka engine table 接 risk.decision.v1 topic
- `risk.risk_decision_kafka_mv` materialized view → audit
- `risk.merchant_fraud_5min` view（运营常用）

## Flink jobs

`deploy/flink/README.md` 推荐 4 个任务：
1. `fraud_rate_per_merchant_5min`
2. `device_velocity` (cross-merchant)
3. `ip_geo_anomaly`
4. `cohort_vintage_curve`

具体作业代码生产应放独立 ML 仓库。

## NebulaGraph schema

`deploy/nebulagraph/schema.ngql` — 启动后用 `nebula-console -f` 跑一次。

## 监控

- `deploy/monitoring/prometheus.yml` — scrape 配置
- `deploy/monitoring/alerts.yaml` — 13 条 alert（per-tier）
- `deploy/monitoring/grafana-dashboard.json` — 主面板（Screen 延时 / verdict mix / ML score / Outcome coverage / 等）

## Runbook

`deploy/runbook/RUNBOOK.md` — 值班 SOP；每条 alert 对应排查步骤。

## 灾难恢复

- **Audit chain**：FileSink + ChainSinkResume 保证服务重启 chain 续；CH
  长留存 730d；Kafka topic 7d 配置可调长
- **Counter**：Redis backend；replica > 1 + AOF 持久化
- **Webhook DLQ**：MemDLQStore 重启清；生产建议补 PG-backed store (schema
  在 internal/webhook/dlq.go 注释)
- **NebulaGraph**：3-node cluster 默认；replica_factor=3；自带 raft 保证

## CI/CD

risk-manage 的 GitHub Actions / GitLab CI 没在本仓提供（公司 platform）。
要点：
- `go test -race ./...` 跑全 31 packages
- `go vet ./... && golangci-lint run`
- `govulncheck ./...` 阻断漏洞
- 镜像 build：`docker build -t ghcr.io/xiongwp/risk-manage:$GIT_SHA .`
- helm upgrade：argocd / flux / 手动 helm upgrade

## 故障演练

`cmd/chaos/chaos.sh` 集成 monthly cron / 手动跑：
- `redis-restart`：杀 Redis 验证 LinkStore warmup
- `risk-restart`：滚动重启验证 rule_count 不变
- `review-restart`：验证 review queue 持久化
- `probe`：synthetic mismatch 率
