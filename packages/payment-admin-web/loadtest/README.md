# payment-admin-web/loadtest

端到端压测，跑在 `payment-admin-web/deploy.sh up` 起的全栈上。测的是从 `split-payment.TriggerEvent` 进、到 `accounting.transaction_order` 落库的端到端 TPS 和错误率。

## 与旧压测的关系

旧压测在 `packages/split-payment/deploy/loadtest/`，自己起 split-payment + etcd，外接 `accounting-system` 单独 compose。问题：

- accounting-system 单独 compose 用单 redis（`accounting-redis`），但 `config.docker.yaml` 配的是 sentinel 模式 → 不兼容
- 跟 payment-admin-web 共享 db（DB-split Batch 6）的方向不一致

新压测复用 `payment-admin-web/deploy.sh up` 已经起来的基础设施（shared-meta + shared-shard-0..9 + risk-redis + accounting-service + split-payment + …），只新增一个 loadtest 容器跑客户端。

旧目录**暂时保留**做 dev/debug 用，但 CI 和 baseline benchmark 都切到这里。

## 拓扑

```
loadtest (这个 dir 起的容器)
   │
   │  gRPC TriggerEvent
   ▼
split-payment:9098  ←─── payment-admin-web/deploy.sh up 起的（golang:1.25 bind-mount monorepo 源码）
   │
   │  gRPC CreateTransaction（按 leg 串行 + abort-on-failure）
   ▼
accounting-service:50051  ←── payment-admin-web/deploy.sh up 起的（复用 risk-redis 单实例）
   │
   │  TCC try/commit
   ▼
shared-shard-0..9 (mysql)  ←── deploy/shared-db/ 起的（accounting + split-payment 共用 schema）
   │
   └─ accounting_db_N.transaction_order_NN  (端到端 ground truth)
```

## 用法

### 前置（只跑一次）

```bash
docker network create payment-stack
export GITHUB_TOKEN=ghp_xxx                          # 私仓 build 必需

cd packages/payment-admin-web
bash deploy.sh init-kms                              # 首次：产 KMS master key（已有可跳过）
bash deploy.sh up                                    # 起全栈，60-120s（含 build）
bash deploy.sh status                                # 看每个服务在不在
bash deploy.sh check                                 # tcp 探活各 gRPC 端口
```

### 跑压测

```bash
cd packages/payment-admin-web/loadtest
./scripts/run.sh                            # 默认：mixed × 60s × concurrency=100
./scripts/run.sh --duration=30s             # 缩
./scripts/run.sh --mode=transfer            # 单跑 transfer（1 leg baseline）
./scripts/run.sh --concurrency=200          # 加并发
./scripts/run.sh --fresh                    # 强制重建账户池
./scripts/run.sh --skip-verify              # 跳过最后的 mysql 落账验证
```

### 看结果

- 报告 JSON：`output/loadtest-report.json`（含 p50/p90/p95/p99 latency + 错误分布）
- 完整日志：`/tmp/payment-loadtest-<timestamp>.log`
- 账户池：`output/account_pool.json`

## 单步调试

如果想跳过 `run.sh` 一步一步来：

```bash
COMPOSE="docker compose -f docker-compose.loadtest.yml"

# 1. build
${COMPOSE} build loadtest

# 2. bootstrap 账户池
${COMPOSE} run --rm --entrypoint /usr/local/bin/loadtest-bootstrap loadtest \
  --accounting-grpc=accounting-service:50051 \
  --accounting-http=http://accounting-service:8888 \
  --output=/output/account_pool.json \
  --num-users=1000 --num-merchants=1000 --num-channels=1000 \
  --currency=PHP --workers=32

# 3. 预充值
bash scripts/prefund.sh

# 4. 单跑某个 mode
${COMPOSE} run --rm loadtest \
  /usr/local/bin/loadtest --config=/loadtest/config.yaml --mode=transfer --duration=30s

# 5. 验证
bash scripts/verify.sh
```

## 配置说明

`config.yaml`：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `load.concurrency` | 100 | 并发协程数 |
| `load.duration` | 60s | 单轮压测时长（warmup 之外） |
| `load.warmup` | 10s | ramp 期 |
| `load.mode` | `mixed` | `topup` / `payment` / `transfer` / `withdraw` / `mixed` / `router-only` |
| `load.mix_ratio.*` | 25/35/25/15 | mixed 模式下各 flow 权重 |
| `flow.channel_count` | 1000 | 账户池规模（必须跟 bootstrap `--num-channels` 一致） |
| `flow.user_count` | 1000 | 同上 |
| `flow.amount_minor_range` | [100, 100000] | 单笔金额 |

## 中规模 baseline 预期

机器：MBP M1 / 16GB / 10 个 mysql container + accounting + split-payment + loadtest 全本地

| Mode | 期望 TPS | 期望 p95 |
| --- | --- | --- |
| `router-only` | 1000+ | < 50ms |
| `transfer` (1 leg) | 200+ | < 200ms |
| `topup` (multi leg) | 100+ | < 500ms |
| `mixed` | 150+ | < 500ms |

数字明显低于上面值时优先看：mysql connection pool / accounting TCC commit 锁 / split-payment translator 性能。

## 已知坑

- **bootstrap 卡住**：accounting `CreateAccount` 在第一次跑时会触发账户表创建，几千个账户 ~30-90s 是正常的。如果卡 5min 以上，去看 `docker logs accounting-service` 找 mysql 连接错误。
- **prefund 找不到 account_NN 表**：accounting 还没起完（mysql init SQL 跑了一半）。等 30s 重试。
- **transaction_order 数远小于成功请求数**：多 leg flow（topup / payment / withdraw）一笔请求可能拆 2-3 条 transaction_order。这是预期。

## 文件清单

```
loadtest/
├── README.md                        本文件
├── config.yaml                      场景配置
├── docker-compose.loadtest.yml      只加 loadtest service 到 payment-stack
├── scripts/
│   ├── run.sh                       一键 orchestrator
│   ├── prefund.sh                   shared-shard-N 充值
│   └── verify.sh                    查 transaction_order + 错误分布
└── output/
    ├── account_pool.json            bootstrap 产物
    └── loadtest-report.json         压测产物
```

Go binary 源码在 `packages/split-payment/cmd/loadtest{,-bootstrap}/main.go`（loadtest 跟 split-payment 强耦合，留在那边方便 import 共享 domain 类型）。Dockerfile 在 `packages/split-payment/deploy/loadtest/Dockerfile.loadtest`，被这里 `docker-compose.loadtest.yml` 引用。
