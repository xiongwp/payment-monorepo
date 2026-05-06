# 千万级用户上线综合评估报告

**评估范围**：payment-monorepo 全 13 个服务  
**目标场景**：10M MAU / 500K-1M DAU / 5K-10K TPS 峰值  
**评估时间**：2026-05-06  
**评估维度**：资金安全 · 系统稳定性 · 可扩展性 · 可维护性 · 安全合规

---

## 一句话结论

**当前状态：不能直接上线 10M 用户。**

按现状能稳定承载 **2-3K TPS / ~500K MAU**。要扩到 10M，**不需要架构重构**，但需要补齐 **8 个 P0 阻塞项 + 9 项 P1 容量配置**。预计 4-6 周达到生产级就绪（99.95% SLO）。

整体设计基础扎实——分片、Outbox、KMS、审计链、配置中心都已就位。短板集中在 **跨服务原子性补偿、关键链路降级路径、运维告警闭环、admin 真鉴权**。

---

## 维度评分卡

| 维度 | 评分 | 现状 | 关键短板 |
|---|---|---|---|
| 资金安全 | 7/10 | 幂等/Outbox/审计/精度都到位；缺跨服务原子性补偿 + 对账告警 | order→outbox→accounting 失败无补偿；reconplatform 检测不报警 |
| 系统稳定性 | 6/10 | 熔断/限流/退避/优雅停机就位；缺降级路径 + canary 框架 | payment-core ↔ channel 失败无降级；merchant 维度限流缺 |
| 可扩展性 | 7/10 | 分片/无状态/异步化都就位；缺关键缓存层 | KMS Decrypt 无 Redis cache (单实例 500 ops/s)；audit 仍在 meta |
| 可维护性 | 8/10 | 日志/指标/SOP/CI 都成熟；缺 alertmanager 规则 | 无 burn-rate alert；OTel 默认未启用；outbox lag 指标缺 |
| 安全合规 | 7/10 | PCI 单跳/KMS/审计链/限流都到位；缺 mTLS 全局 + 真鉴权 | api-gateway↔user-merchant 走 insecure；config-center admin 是 stub |

**综合：7.0/10 — 距生产级就绪还有 3-4 周工作量。**

---

## 资金安全（最关键维度）

### ✅ 已就位

- **幂等性**：`payment-core/internal/service/idem.go` 用 `sha256(pi_id‖action‖amount‖currency)` 作 key；`payment-channel` 表 UNIQUE(idempotency_key) 强约束。历史 bug（半价券导致 hash 碰撞）已修。
- **Outbox**：`order-core/internal/domain/accounting_outbox.go:44` request_id UNIQUE + claim_token 防 lease 抢占；自适应轮询 + Wake 信号最大延迟 ≤10s。
- **审计链**：`card-center/internal/audit/audit.go` per-shard hash chain，10×100 分片，启动期续接 prev_hash，Kafka + DB 双落 7 年留存。
- **金额精度**：全栈 int64 minor_units，float bug（maya adapter 历史 case）已修复。
- **风控决策**：DecisionAudit 落库 + replay 工具支持规则集 A/B 验证。

### ❌ P0 阻塞项

1. **跨服务原子性无补偿**（最严重）  
   现状：order-core PI/Charge → outbox → accounting ledger 三步分离；outbox 投递失败仅靠 retry，**没有定时修复 / 兜底补偿任务**。极端 case 下 ledger 余额会与 PI 状态永久不一致。  
   修复路径：补 reconcile worker（accounting 端按 outbox.processed=false 且 created_at < now-1h 反查 order-core，触发对账修复）。工期 3-5 天。

2. **对账无告警**  
   `reconplatform/internal/engine` 能检测 payment vs ledger 差异（Redis Lua UpsertEvent），但**差异检测后只落表无告警**；需补 Prometheus exception_count 指标 + alertmanager 规则。工期 1-2 天。

3. **mTLS 不全局强制**  
   `payment-channel` 对外强制，但 `api-gateway ↔ user-merchant-core` 卡支付链路用 `insecure.NewCredentials()`，只靠 token 鉴权。生产环境必须全链路 mTLS。工期 2 天。

4. **Webhook 签名缺 trace_id**  
   X-Signature HMAC-SHA256 已就位，但 header 没有 X-Trace-ID，故障排查只能靠 webhook 事件 ID 回查日志（手工跨服务 grep）。工期半天。

### ⚠️ 风险点（非阻塞但需警觉）

- 跨币种汇率精度：仅 int64，无 Decimal 库；纯 PHP/USD 结算 OK，要进多币种结算需补 `shopspring/decimal`。
- Risk audit 不含 IP/device fingerprint：replay 时输入特征不全，可能放过现网真欺诈。

---

## 系统稳定性

### ✅ 已就位

- **熔断 + 限流**：adapter 粒度熔断（visa/mastercard/maya 各自独立），IP 维度 token bucket，risk-manage 默认 fail-policy=close（启动期强制 guard）。
- **超时 + 退避**：gRPC deadline 全链路 propagate，webhook 重试 [0s/30s/2m/15m/2h] 指数退避。
- **优雅停机**：35s drain timeout，liveness/readiness 分离，outbox poller 看 ctx.Done() 干净退出。
- **副本数 + rolling update**：核心服务都 ≥3 副本部署。

### ❌ P0 阻塞项

1. **payment-core 对 payment-channel 失败无降级**  
   场景：channel 对接的卡组织（如 Visa）抖动 → adapter 熔断打开 → payment-core 收到 unavailable → **直接 5xx 给上游**（order-core/api-gateway/用户）。没有「降级到备用渠道」「写 pending + 异步重试」「友好错误码」之一。  
   修复：channel router 加备用路由（visa 挂时尝试 mastercard）+ payment-core 认 `unavailable` 类错误时 enqueue retry queue 而不是直接 fail。工期 5-7 天。

2. **merchant 维度限流缺失**  
   现状：rate limiter 抽象就位但只挂了 IP 维度。一个流量异常（卡测试/抓 promo）的 merchant 可以打爆全平台 outbox + accounting。  
   修复：rate_limit middleware 加 merchant_id 维度（config-center 配置每商户配额）。工期 2-3 天。

3. **config 变更无 canary 框架**  
   config-center 已支持 CANARY/TARGETED 策略字段，但 **risk-manage / payment-channel 等关键服务的 SDK 没有按 instance_id 落入 CANARY 桶**——意味着改一个 key 就是全集群秒推。某条规则配错 → 1 秒内全部 risk-manage 副本拒单。  
   修复：SDK 加 CanarySpec 命中判定 + payment-admin-web admin 推送时默认 1% canary。工期 3-5 天。

### ⚠️ 风险点

- **Webhook 重试缺 jitter**：[0s/30s/2m/15m/2h] 是固定步长，下游集群抖动会导致重发突刺；加 ±20% jitter 即可。
- **熔断粒度粗**：adapter 级，没做 merchant×channel 矩阵；某商户在 visa 上出事不应熔掉所有商户的 visa。
- **config-center 单副本**：v1.0 只起 1 个；挂了 SDK 走本地 cache 兜底，但热更新断了。生产建议 3 副本 + etcd 多端点。

### SLO 估算

修完 3 个 P0 + jitter + config-center HA → **可达 99.9% SLO**。  
全部修完（含 P1 熔断细化、merchant×channel 矩阵）→ **99.95% SLO**。

---

## 可扩展性

### 容量基线

| 配置 | TPS 上限 | MAU 等价 | 限制因素 |
|---|---|---|---|
| 当前现状 | **2-3K** | ~150K | KMS Decrypt 单实例 500 ops/s + bulkhead per_merchant_max=256 |
| 加 KMS Redis cache | ~5-6K | ~300K | user_session 30K SELECT/sec + outbox 单 worker |
| 完成 9 项 P1 | **10K+** | **1M-10M** | 无单点瓶颈 |

### ✅ 已就位

- **分片**：order-core 10×10=100 shard（按 payment_intent_id 位编码），user-merchant-core 10×10，按位编码 ID 已落地。
- **路由**：Router V1 + Resharding SOP（docs/RESHARDING.md）规划支持 1000 shard。
- **无状态**：api-gateway / payment-core 完全无状态；后台 worker 用分布式锁 leader-only。
- **异步化**：outbox 5s poll，Kafka 事件去重，关键路径 ≤6 跳 mTLS（30ms 基线）。
- **ID 生成**：Leaf-segment buffer 异步预取，跨服务 ID 段位编码隔离。

### ❌ P1 容量瓶颈（9 项配置 + 一项数据迁移）

1. **KMS Decrypt 加 Redis cache**（最关键）：5min TTL，命中率预期 80%+，单实例 500→2.5K ops/s；不加直接卡死在 2-3K TPS。
2. **IntrospectToken 加进程内 LRU**：10K 容量、30s TTL；命中率 95%+，user-merchant DB QPS 30K→1.5K。
3. **DB max_open_conns**：order/payment 库从 50→200，user-merchant 200→100。
4. **gRPC MaxConcurrentStreams**：100→2000 默认。
5. **Accounting outbox worker 并发**：1→8/shard。
6. **Redis client pool**：10→100。
7. **gRPC 连接池**：1→10/upstream。
8. **Bulkhead per_merchant_max**：256→1024（头部商户实测 ~400 TPS）。
9. **audit_log 迁 10 shard**：当前在 meta 库，30K writes/s 接近 MySQL 单实例上限；按 merchant_id 迁分片即可。

### ⚠️ 待规划

- **读写分离的应用层路由策略**：MySQL 主从拓扑已就绪，但应用没标记哪些 SELECT 可走 replica。批量查询 / dashboard / reconplatform 适合走 replica，可削主库 30-40% 读压力。
- **Resharding Phase 1 设计评审**：当 100 shard 单 shard QPS > 5K 时启动；目前规划好但没演练过。

---

## 可维护性 + 可观测性

### ✅ 已就位

- **日志**：全栈 zap structured JSON；trace_id 全链路（UnaryInterceptor 自动注入）；PAN/CVV 自动 mask。
- **指标**：每服务 /metrics 暴露 RED；order-core/user-merchant/card-payment/accounting 都有业务指标（success_rate by channel）。
- **链路**：OTel 框架就绪，gRPC/HTTP 自动 span；trace_id W3C 标准 + 兼容 x-trace-id 头。
- **配置管理**：config-center 全平台收口（13 服务都接 SDK）；OnChange 热更新 + atomic.Pointer 防锁；本地 cache fallback 不阻塞业务。
- **CI / 部署**：deploy.sh 一键启停；Dockerfile 多源 GOPROXY（proxy.golang.org → goproxy.cn → direct） + 5x 重试；init SQL 自动导入 + wait_all_mysql；BuildKit additional_contexts 支持本地兄弟仓。
- **测试**：40+ test 文件；payment-util/replay 自动化压测框架；e2e 覆盖核心流程。
- **SOP**：Resharding SOP（4 阶段）+ 5DIM 代码深扫报告。

### ⚠️ 必修

1. **告警规则缺失**：SLO 已定义（docs/SLO.md：99.95% 可用、p99<5s），但 **无 alertmanager rules 文件**。需补 burn-rate alert（短窗 1h × 14.4×SLO + 长窗 6h × 6×SLO）+ on-call page 通道。工期 2-3 天。
2. **outbox lag 指标缺**：标记过但未落 Prometheus 指标，故障时无法快速定位消息堆积。工期半天。
3. **OTel 默认未启用**：需 `OTEL_EXPORTER_OTLP_ENDPOINT` 环境变量；建议 dev/staging/prod 都默认指向 Tempo/Jaeger。工期半天。
4. **idempotency 表清理 cron 缺**：长期会膨胀，需定时 TRUNCATE 90 天前的旧记录。工期 1 天。

---

## 安全合规

### ✅ 已就位

- **PCI 合规**：PAN 单跳 card-center HTTPS 直连；api-gateway 完全隔离无 PAN 字段（只见 stored_token + masked_pan + last4）；HSTS 生产启用。
- **审计链**：链式 SHA256 hash + 10×100 shard append-only；actor + ip + reason 字段必填。
- **限流**：3 层（IP / merchant / per-service）；登录/OTP/注册/改密接口都过限流防爆破。
- **SQL 防护**：全 ORM gorm 参数化；form binding 白名单。
- **KMS**：master key 文件加载 + 90 天过期告警 + key rotation 字段就位。
- **CSRF**：admin web Double-Submit Cookie + SameSite=Lax（最近修复）。

### ❌ P0 阻塞项（必修才能上线）

1. **mTLS 不全局**：api-gateway ↔ user-merchant-core 卡支付链路用 `insecure.NewCredentials()`。生产强制全链路 mTLS（含 SAN 白名单严格匹配）。工期 2-3 天。
2. **config-center admin 是 stub**：现在用 X-Admin-User header + env 兜底 `admin@localhost`。生产前必须替换成真 SSO（mTLS gRPC 调 user-merchant-core IntrospectToken 验 admin role）。工期 3-5 天。
3. **KMS key rotation 无自动化**：当前只告警，必须手工重启容器才加载新 key。需补 SDK rotate 推送（同一密钥保留 N 个版本，解密兼容前一版本）。工期 5-7 天。
4. **govulncheck 未集成 CI**：Go modules 没有自动漏洞扫描。工期半天。

### ⚠️ 风险点

- 越权检测在多个 BFF 端点已加（card-center DeleteCardByID + 3 道身份防线），但 order/payment-core 的端点是否每条都查 merchant_id 隔离？需做一次 BFF endpoint 矩阵扫。工期 2-3 天。

---

## 上线路线图

### 阶段 1（2 周）：P0 阻塞项

并行 3 个 owner：

**Owner A（资金安全 + 稳定性）**
- 跨服务原子性补偿 worker（accounting 反查 outbox 修复）
- 对账告警（reconplatform 暴露 Prometheus + alertmanager rule）
- payment-core 降级路径（channel 备用路由 + retry queue）
- merchant 维度限流

**Owner B（容量 + 配置）**
- KMS Decrypt Redis cache + 3 副本
- IntrospectToken LRU cache
- DB conn pool / gRPC stream / outbox worker 并发批量调
- bulkhead per_merchant 256→1024
- audit_log 迁 10 shard（带数据迁移演练）

**Owner C（安全 + 可观测）**
- mTLS 全链路（api-gateway ↔ user-merchant-core）
- config-center admin SSO 鉴权
- alertmanager 规则（burn-rate + outbox lag + KMS cache hitrate）
- govulncheck CI

### 阶段 2（1 周）：压测 + 灰度

- 压测 5K TPS / 1h（payment-util/replay 跑历史流量）
- 验证 KMS hitrate > 80% / outbox lag < 30s / p99 < 5s
- 灰度 1% 流量 7 天
- 压测 10K TPS / 30min 终验

### 阶段 3（1 周）：升满量 + 持续观察

- 100% 流量切换
- 7×24 监控 SLO 指标
- 双周复盘 + 调参

### 阶段 4（持续）：长期演进

- config-center canary 框架
- KMS 自动 key rotation
- Resharding Phase 1 演练
- 越权检测端点矩阵 + 红队渗透
- 应用层主从读写分离

---

## 关键监控指标 SLO（上线后）

```yaml
# 平台级 SLO
availability:        99.95% (monthly)  # > 21min downtime
p99_authorize:       < 5000ms
p99_charge:          < 3000ms
error_budget_burn:   < 14.4× SLO over 1h  → page
                     < 6.0× SLO over 6h   → page

# 关键资源指标
outbox_lag_seconds:           < 30s    (critical: 60s)
bulkhead_saturation:          < 70%    per merchant×channel
kms_decrypt_cache_hitrate:    > 80%
introspect_token_hitrate:     > 95%
db_pool_utilization:          < 80%
single_shard_qps_p95:         < 5000

# 业务指标
auth_success_rate:            > 95%    by (channel, country)
3ds_challenge_rate:           ~5-15%   (异常波动告警)
ledger_reconcile_diff:        = 0      pending: > 1h → page
risk_review_queue_p95_age:    < 30min
```

---

## 风险矩阵

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| 跨服务事务失败 ledger 不一致 | 中 | 极高 | reconcile worker（阶段 1） |
| Channel 抖动级联失败 | 高 | 极高 | 备用路由 + 降级（阶段 1） |
| 单 merchant 流量打爆全平台 | 中 | 高 | merchant 限流（阶段 1） |
| Config 误推全集群 | 中 | 极高 | canary 框架（阶段 4） |
| KMS 单点 + 缓存击穿 | 高 | 高 | Redis cache + 3 副本（阶段 1） |
| Audit log 写入瓶颈 | 高 | 中 | 迁 10 shard（阶段 1） |
| Admin web 无真鉴权 | 低 | 高 | SSO 接入（阶段 1） |

---

## 总结

**这不是从零搭新系统的项目**——基础架构（分片、Outbox、审计链、配置中心、KMS、风控、PCI 单跳）都做得很扎实，主要差距在 **生产级补偿/降级路径** 和 **运维闭环（告警 + 容量缓存）**。  

按上面的路线图，**4-6 周可以达成 10M 用户 99.95% SLO**。最大风险不是技术债，是没有压测验证过 5K-10K TPS 场景的真实表现——所以阶段 2 的压测一定要全链路真跑（payment-util/replay 重放历史流量），不能只跑单服务 micro-benchmark。

**立刻能做的事**（不阻塞别人）：
1. 装 alertmanager 规则文件 + 接 PagerDuty
2. KMS Decrypt 接 Redis cache
3. mTLS 全链路（先把 insecure.NewCredentials() 灰名单列出来，逐一替换）
4. govulncheck 加进 CI

不建议立刻做的事：
- 重构现有架构（方向是对的，先把生产级补偿和降级补齐）
- 拆更多分片（现在 10×10 还有充足容量空间）
- 引入新中间件（先把已有的 etcd/Kafka/Redis 用透）
