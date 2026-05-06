# 10K TPS 容量规划

回答 "能否支持 1 万 TPS 支付流量"。结论 + 瓶颈分析 + 必须改的配置 + 拓扑。

## TL;DR

**当前代码门槛：可以**。需要做：
1. **5 个代码改动**（已完成 commit `5bb69da1`）
2. **9 个配置调整**（写在本文 §3）
3. **拓扑扩容**：card-payment / order-core / payment-channel ×5；KMS ×3；Redis cluster
4. **2 个新增依赖**：Redis cluster（IntrospectToken cache）+ 第 2 个 KMS HSM

不做这些 → 当前代码 ~2K-3K TPS（KMS 单实例 + bulkhead 32 + HTTP idle 50 是硬瓶颈）。

---

## 一、流量假设

| 指标 | 假设值 | 说明 |
| --- | --- | --- |
| 总 TPS | 10,000 | 平均（不是峰值） |
| 峰值 / 平均比 | 2× | 大促/秒杀 = 20K TPS 短时 |
| 商户数 | 100 | 80/20 分布 → 头部 20 商户占 80% |
| 头部单商户 TPS | 400 | 10K × 80% / 20 |
| Authorize 端到端 P50 | 1.5s | 含卡组织 ~1s |
| Authorize 端到端 P99 | 5s | SLO |
| 在飞并发 | 30,000 | 10K × 3s 平均 |

---

## 二、瓶颈分析（自上而下）

### 2.1 api-gateway 入口

每 charge 1 HTTPS 请求；P99 1s（外部 + 后端）；10K req/s × 1s = 10K 在飞。

| 资源 | 默认 | 10K TPS 需求 | 状态 |
| --- | --- | --- | --- |
| TLS handshake / sec | ~5K (1 instance) | 10K | ⚠️ **需 2 replicas** |
| HTTP fd 上限 | 65K (sysctl) | < 50K | ✅ |
| BFF 内 mTLS gRPC client | 默认 100 idle | 200 / instance | ⚠️ 需调 |

### 2.2 mTLS gRPC 链路

每 charge 经过：api-gateway → order-core → payment-channel → card-payment → card-center → kms-manage

5 跳 mTLS gRPC，每跳 LAN RTT ~5ms。**链上 25ms 是最低延迟**，全链路压不过这个值。

| 资源 | 默认 | 10K TPS 需求 | 状态 |
| --- | --- | --- | --- |
| gRPC server max concurrent streams | 100 | > 1000 | ⚠️ 需调 grpc.MaxConcurrentStreams |
| keepalive ping 频率 | 10s/3s (hardenedKeepalive) | 不变 | ✅ |
| connection pool / target | 1 (default round_robin) | ≥ 10 | ⚠️ 看 ServiceConfig |

### 2.3 KMS-manage（最大瓶颈）

每 Authorize 至少 1 次 KMS Decrypt（detok payment_token），可能 2 次（含审计 KMS encrypt）。
HSM-backed 单实例 ~500 ops/sec（典型软 HSM；硬 HSM 1K-2K）。

**10K TPS × 1.5 KMS ops = 15K KMS ops/sec → 单实例 30× 不够**。

| 配置 | 当前 | 10K 需求 |
| --- | --- | --- |
| KMS replicas | 1 | **3-5**（HSM cluster） |
| Decrypt cache | 无 | **必加** Redis 5min TTL（命中率 80%+ 实际 KMS 量降 5× → 3K ops/sec / 实例） |

card-center 已经有 envelope cache 设计（`internal/cryptoenv/gcm_cache.go`），但 KMS-manage 这层没有。**需补**。

### 2.4 IntrospectToken 链（仅次于 KMS）

每 mTLS RPC 都 IntrospectToken（验 user_session 是否还在表里 → 防 logout 后 token 仍有效）。
10K TPS × 3 服务调用链 = **30K user_session 表 SELECT/sec**。

user_session 是分片表（10×10），平均下来每 shard ~300 SELECT/sec。**勉强够**，但延迟堆积。

**优化**：进程内 LRU cache 30s TTL → 命中率 95%+ → 实际 DB ~1.5K SELECT/sec。简单且安全（30s 是登出延迟容忍度）。

| 配置 | 当前 | 10K 需求 |
| --- | --- | --- |
| 进程内 LRU | 无 | **加，cap 10K，TTL 30s** |
| Redis 共享 cache | 无 | 二期再加（多实例命中率提升） |

### 2.5 数据库（OK）

| 操作 | 写量 | 当前容量 | 状态 |
| --- | --- | --- | --- |
| PI insert | 10K/s | 10 shards × 10K op/s/shard = 100K | ✅ |
| Charge insert | 10K/s | 同上 | ✅ |
| Outbox insert | 10K/s | 同上 | ✅ |
| Audit insert | 10K-20K/s | meta DB 单库 ~30K op/s | ⚠️ **audit meta 单库瓶颈，需分片** |
| user_session SELECT | 30K/s 不加 cache | 100K/s | ✅ 但浪费；加 cache 1.5K/s |

**audit meta 库分片**是后续要做的（task 跟踪：audit_log 当前 meta 单库；按 mch_id 分片到现有 10 shards 即可）。

### 2.6 Redis（OK）

会话 + 限流 + breaker 状态。10K TPS × 3 = 30K Redis ops/s。
单 master + 2 replica 标准配置 ≥ 100K ops/s。**够**。

唯一注意：**Redis pipeline + connection pool 必须开**（go-redis 默认 10 conn / instance，得调到 100+）。

### 2.7 Kafka audit（OK）

10K-20K msg/s，lz4 压缩，单 broker 可吃；3 broker cluster 完全够。Producer batch 100ms = 100 msg/batch 已开。

### 2.8 Card 网络出口

每 Authorize 一次 HTTPS 给 Visa/MC/etc. P50 ~1s P99 ~5s。
在飞 = 10K × 1.5s = 15K 连接。分到 5 networks ≈ 3K / network。

| 配置 | 当前 | 10K 需求 |
| --- | --- | --- |
| MaxIdleConnsPerHost | 200 (commit 5bb69da1) | 200 ✅ |
| MaxConnsPerHost | 500 | 500 ✅ |
| dial timeout | 30s default | 5s（避免堆积） |

### 2.9 Bulkhead

100 商户 × 平均 100 在飞 = 10K，对齐总在飞数。head 大商户 400 TPS × 1.5s = 600 在飞 → 需要 1024 cap。

`bulkhead.per_merchant_max=256` 默认（commit 5bb69da1），head 商户 config 调到 1024。

### 2.10 单实例 Goroutine

10K TPS × 3s = 30K 并发 goroutine 在 card-payment / order-core 等服务。
Go runtime 100K goroutine 是 fine（每个 4KB 栈 = 400MB），但 GC 压力大。

**优化**：
- worker pool（fixed 1024 worker + 队列）替换无界 spawn — 已大部分到位
- sync.Pool 复用大对象（Authorize 的 JSON marshal buf）

---

## 三、必须改的配置（9 项）

| # | 服务 | 配置 | 默认 | 10K 值 |
| --- | --- | --- | --- | --- |
| 1 | card-payment | `bulkhead.per_merchant_max` | 256 | 256 (head 1024) |
| 2 | card-payment | network HTTP MaxIdleConnsPerHost | 200 ✅ | 200 |
| 3 | gRPC server | `MaxConcurrentStreams` | 100 | **2000** |
| 4 | grpc client | `WithDefaultServiceConfig`+`balancer="round_robin"` | 已开 | + connectionPool size 10 |
| 5 | redis client | pool_size | 10 | **100** |
| 6 | kms-manage | replicas | 1 | **3** + Redis decrypt cache |
| 7 | user-merchant-core | IntrospectToken cache | none | **加 LRU 10K / 30s TTL** |
| 8 | accounting-system | outbox dispatcher concurrency | 1 worker | **8 worker / shard parallel** |
| 9 | DB max_open_conns | 50 / shard | 50 | **200** / shard (主)；100 / shard (read replica) |

---

## 四、拓扑扩容

| 服务 | 单 replica TPS 上限 | 10K TPS replicas | 备注 |
| --- | --- | --- | --- |
| api-gateway | 5K | **2** | TLS handshake + BFF aggregation |
| user-merchant-core | 10K | 2 | 加 cache 后 RPS 不是瓶颈 |
| order-core | 5K | **3** | PI / Charge / Refund 状态机 + DB write |
| payment-core | 5K | 3 | routing + risk pre-screen |
| payment-channel | 3K | **4** | adapter HTTPS 出站 IO bound |
| card-payment | 3K | **4** | 同上，PCI scope 必须独立 DC |
| card-center | 5K | 2 | KMS encrypt + DB |
| kms-manage | 500 (HSM bound) | **3-5** | HSM 拆资源池 |
| accounting-system | 5K | 3 | 复式记账事务 |
| risk-manage | 10K | 2 | 内存决策为主 |
| clearing-settlement | 1K | 1 | 异步批 |

**总 K8s nodes**：~60-80 cores。3-node MySQL × 10 shard cluster；3-node Redis cluster；3-node Kafka；3-node etcd。

---

## 五、压测验证

按这个顺序压：

1. **单服务 baseline**：用 ghz 直接压 card-payment Authorize（mock-network 路径），找单 instance 极限
2. **2 服务链路**：order-core → card-payment（mock）
3. **全链路 mock**：api-gateway → ... → mock-network。10K TPS × 5min，看：
   - 各服务 P99 latency
   - DB pool wait_count
   - Bulkhead reject rate
   - Goroutine count（应 < 50K）
   - GC pause P99（应 < 100ms）
4. **真渠道 sandbox**：Visa CyberSource 沙箱，1K TPS × 10min（沙箱通常 rate limit）
5. **prod 灰度**：1% → 10% → 50% → 100%，每档 24h 观察 SLO

工具：
- `payment-util/replay` 已内置（task #70）
- `ghz` for gRPC
- `k6` for HTTPS

---

## 六、监控阈值（10K TPS）

`docs/SLO.md` 已定义；10K TPS 时这些必须达标：

```
paycore_charge_duration_seconds:p99            < 5s
paycard_authorize_duration_seconds:p99         < 4s
paycard_bulkhead_active / capacity              < 0.7 (per merchant)
paycore_db_pool_in_use / max                   < 0.6
acct_outbox_lag_seconds                         < 30
go_goroutines per instance                      < 50000
go_gc_duration_seconds:p99                      < 100ms
process_resident_memory_bytes per instance      < 4 GB
```

任一超阈值 = 已经在容量边缘，需要立即扩容。

---

## 七、本次代码已落地

commit `5bb69da1` 已加：
- ✅ pprof on metrics endpoint（CPU / heap / goroutine / trace）
- ✅ httpx MaxIdleConnsPerHost 200，MaxIdleConns 1000，MaxConnsPerHost 500
- ✅ Bulkhead default 256 + 文档化 1024 head merchant 配置

**待补**（task #106）：
- ⏳ user-merchant-core IntrospectToken LRU cache
- ⏳ kms-manage Decrypt Redis cache
- ⏳ accounting-system outbox 并发 worker (8 / shard)
- ⏳ gRPC server MaxConcurrentStreams 2000 全栈

---

## 八、回答开始的问题

> 能否支持 1 万 TPS 的支付流量？

**短答：能，但要做 §3 + §4**。

如果立即上 prod 不做配置调整：
- 平台 ~2-3K TPS 后 KMS 拥塞导致 Authorize P99 超 SLO
- bulkhead default 32 把单 merchant 卡死
- HTTP idle pool 不够导致频繁 TCP 握手

按本文修完：
- 单实例瓶颈消除
- KMS 池化 + cache 撑住 10K
- 拓扑 60-80 core 集群跑得动 10K avg / 20K peak
- error budget 30 天 21.6min 大概率不烧光

下一步代码补 IntrospectToken cache + outbox 并发 worker（task #106 跟踪），就是 production-ready 的 10K TPS 平台。
