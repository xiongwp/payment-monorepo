# 轮换账户 End-to-End 压测套件

测试目标：在真实 Docker 部署（accounting-system + split-payment + MySQL/Redis/etcd）上压测 4 大资金流，量出真实生产 TPS。

## 架构

```
┌──────────────────┐     gRPC     ┌──────────────────┐     gRPC      ┌─────────────────────┐
│  loadtest        │ ───────────▶ │  split-payment   │ ────────────▶ │  accounting-system  │
│  (Go client)     │              │  :9098           │                │  :50051             │
└──────────────────┘              └──────────────────┘                └─────────────────────┘
                                                                              │
                                                                              ├─ etcd (svc discovery)
                                                                              ├─ MySQL × 11 (10 shards + meta)
                                                                              ├─ Redis (hot path cache)
                                                                              └─ Kafka (outbox events)
```

可选模式：loadtest 也支持**直连 accounting-system**（跳过 split-payment），用于测量 router 层（rotation 特性）的纯 TPS 上限。

## 前置条件

1. Docker + Docker Compose 已装
2. `payment-stack` Docker 网络已创建（与其他服务共享）：
   ```bash
   docker network create payment-stack
   ```
3. 必要的兄弟仓在本地（`payment-util`, `accounting-grpc-api`）—— 它们通过 `additional_contexts` 注入到镜像构建

## 一键启动

```bash
cd packages/split-payment/deploy/loadtest

# 0. 启动整套（accounting + split + 中间件）
./scripts/run.sh

# 1. 跑压测（默认 60s, 10 并发, 混合流量）
./scripts/run.sh --loadtest

# 2. 单独跑某种资金流
./scripts/run.sh --loadtest --mode=topup --concurrency=20 --duration=120s

# 3. 清理
./scripts/teardown.sh
```

## 资金流类型

| `--mode` | 资金流 | 每笔记账触发的 gRPC 调用 |
| --- | --- | --- |
| `topup` | 用户充值 | 1× MoneyFlow（多 leg）|
| `payment` | 余额支付 | 2× DoubleEntryBooking（TCC Try + Confirm）|
| `transfer` | 用户转账 | 1× DoubleEntryBooking |
| `withdraw` | 用户提现 | 1× MoneyFlow |
| `mixed` | **默认** 30% topup + 40% payment + 15% transfer + 15% withdraw | — |
| `router-only` | 直接调 accounting.RouterDryRun | 测 Router 路由层纯逻辑 |

## 配置（`config/loadtest.yaml`）

```yaml
target:
  mode: "via-split-payment"     # via-split-payment | direct-accounting
  split_payment_endpoint: "split-payment:9098"
  accounting_endpoint: "accounting-system:50051"

load:
  concurrency: 10                # 并发 goroutine 数
  duration: 60s                  # 压测时长
  rate_limit_qps: 0              # 0=不限速
  mode: "mixed"                  # 4 种 + mixed + router-only
  mix_ratio:
    topup: 30
    payment: 40
    transfer: 15
    withdraw: 15

flow:
  channel_count: 100             # 模拟 100 个渠道
  user_count: 100000             # 模拟 10 万用户
  amount_minor_range: [100, 100000]  # 金额范围（分）

report:
  output_path: "./loadtest-report.json"
  prometheus_pushgateway: ""     # 配置后 push metrics
```

## 输出报告

```
=== loadtest report (60s) ===
Mode:          mixed
Total ops:     58,234
TPS (avg):     970/sec
Errors:        12 (0.02%)

Latency (ms):
  p50:   8.2
  p90:  18.7
  p95:  24.3
  p99:  47.1
  max:  213.5

Per-flow breakdown:
  topup:      17,401 ops  | 296/sec | p99=45.2ms
  payment:    23,318 ops  | 397/sec | p99=52.1ms
  transfer:    8,728 ops  | 149/sec | p99=28.7ms
  withdraw:   8,775 ops   | 150/sec | p99=43.5ms

Top errors:
  - context deadline exceeded: 7
  - rotation: no active instance: 3
  - tcc cancel timeout: 2
```

## 解读 TPS

- **router-only 模式**：测量纯路由逻辑天花板，预期 200K-1M ops/sec（in-memory bench 已测得 2-3M）
- **direct-accounting 模式**：测量包含 DB IO 的 booking 路径，预期 5K-30K bookings/sec
- **via-split-payment 模式**：测量端到端业务流，预期 1K-10K bookings/sec（额外有 split-payment 转换 + workflow 开销）

## 已知问题

- ⚠️ 首次跑前需要 5-10 分钟让 MySQL 完成 100 张 account_NN 表 + 100 张 tx_account_anchor_NN + 100 张 flow_anchor_route_NN 的建表
- ⚠️ 压测前需要预注册 logical_accounts（脚本会自动做）
- ⚠️ 多副本部署时 GITHUB_TOKEN 需要传给 docker build

## 文件清单

| 文件 | 用途 |
| --- | --- |
| `docker-compose.yml` | Compose stack 编排 |
| `Dockerfile.loadtest` | loadtest 镜像构建 |
| `cmd/loadtest/main.go` | loadtest 主程序 |
| `config/loadtest.yaml` | 配置文件 |
| `scripts/run.sh` | 一键起栈 + 跑压测 |
| `scripts/teardown.sh` | 清理 |
| `scripts/bootstrap-logical-accounts.sh` | 预注册 LA |

## 演练 / Chaos 测试

```bash
# 压测中途重启 accounting-system，观察恢复
./scripts/run.sh --loadtest --chaos=restart-accounting --duration=120s

# 压测中途断 MySQL master 30s
./scripts/run.sh --loadtest --chaos=mysql-restart --duration=300s
```
