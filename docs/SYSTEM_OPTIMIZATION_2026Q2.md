# 全系统优化 — 2026Q2

整个支付平台跨 11 服务的安全 / 稳定 / 可维护性现状盘点 + 已落地优化 + 待办。

## 服务列表

| 服务 | 角色 | 入站 | 出站 |
| --- | --- | --- | --- |
| api-gateway | BFF / 用户面 HTTP | 公网 | usermerchant / order-core / risk-manage |
| user-merchant-core | 商户 + 用户中心 | mTLS gRPC | accounting / kms / risk |
| order-core | PI / charge / refund 状态机 | mTLS gRPC | payment-channel / accounting |
| payment-core | 路由 + 风控前置 | mTLS gRPC | payment-channel / risk |
| payment-channel | 渠道适配层 | mTLS gRPC | external 渠道 HTTPS |
| accounting-system | 复式记账 + 账户 | mTLS gRPC | id-generator / Kafka |
| risk-manage | 风控决策 + 黑名单 | mTLS gRPC | ClickHouse / Nebula |
| clearing-settlement | 清结算 + 对账 | mTLS gRPC | accounting |
| reconplatform | 对账平台 | HTTP | DB / Kafka |
| kms-manage | 平台 KMS | mTLS gRPC | HSM (PROD) |
| card-center / card-payment | PCI 卡支付（隔离 DC） | mTLS gRPC | kms / 卡组织 HTTPS |
| id-generator | 全局 ID | gRPC | etcd |

---

## 一、安全（Security）

### 1.1 已落地（高置信度）

| 项 | 实现 | 引用 |
| --- | --- | --- |
| 全栈 mTLS | server: RequireAndVerifyClientCert + client SAN/CN 白名单 | kms-manage:P0-4 + card-center 3 道防线 |
| JWT RS256 | KMS-backed 私钥 + kid rotation 多 key 验签 | authpkg `ee6ff0e6` |
| PAN 单跳 | 浏览器 → card-center HTTPS → KMS envelope 加密；其他服务进程不见 PAN | task #81 |
| AAD 绑定 | stored_token ↔ user_id；payment_token ↔ pi_id；跨绑定立即 ErrAADMismatch | card-center vault |
| KMS Bearer + cert SAN 三层鉴权 | tls 握手 + ClientIdentityInterceptor + AuthInterceptor | kms-manage |
| 审计链 | audit_log row_hash = sha256(prev_hash + canonical_row)，PCI Req 10.7 7 年 | order-core admin_audit_log |
| 审计 Kafka 卸载 | 双写 DB + Kafka (sarama via payment-util/audit/kafkago) | card-center P0-5 + a212df89 |
| Trace + shadow ctx 一致性 | trace.NewBackground 强制 trace_id + shadow=false 给 detached worker | 17 处 worker + 渠道回调 |
| Webhook 签名 | HMAC-SHA256 stripe-style，merchant 拿 webhook_secret 自验 | order-core/internal/webhook |
| OTP 防钓鱼 | cookie path=/verify-otp + 删 query fallback | api-gateway/userweb |
| Refund 幂等 | (pi_id, idempotency_key) 唯一索引 | order-core services.go |
| Webhook claim_token | workerID + atomic seq；多实例无碰撞 | order-core/webhook delivery |
| Async ledger RequestID | 派生 voucherNo，consumer dedup | accounting hybrid_accounting |

### 1.2 待落地

| 项 | 优先级 | 备注 |
| --- | --- | --- |
| ASV 扫描 / pentest / SAQ-D | 🟡 EXTERNAL | 必须找 QSA |
| card-center / card-payment 独立 DC | 🟡 INFRA | 代码已支持，部署层做 |
| API rate limit 标准化 (per-merchant + per-IP) | 🟡 已有但分散 | 收口到 payment-util/ratelimit |
| Cookie SameSite=Strict 切换 | 🟡 兼容期 | 分阶段灰度 |

---

## 二、稳定性（Stability）

### 2.1 已落地

| 项 | 实现 | 引用 |
| --- | --- | --- |
| 优雅关停 | BeginDrain → /readyz 503 → K8s 摘流 → GracefulStop | payment-core / card-payment metrics |
| DB 连接池监控 | OpenConns / Idle / Wait 全栈 Prometheus | task #8 |
| 熔断器 | risk-manage / payment-core (network) / **card-payment 5 networks 各一** | 472547fc |
| Outbox lag metrics | acct_outbox_lag_seconds | task #67 |
| gRPC keepalive | hardenedKeepalive 10s/3s + ServerEnforce 5s | payment-util/serviceregistry |
| 熔断 fail-policy 显式 | risk-manage allow / deny / fail_open / fail_close | task #64 |
| Reconcile worker | card-payment + payment-channel pending_query + order-core charge_reconcile | P0-2 + 3 处 |
| Two-step claim 防多 worker race | 给 webhook delivery / charge_workers / freeze_compensate | task #9 + #66 |
| Read context.Background 审计 | trace.NewBackground 17 处 worker 改造 | docs/CTX_BG_REFACTOR.md |
| Idempotency: refund / pi.create / outbox / webhook | 全部唯一键 + 入参 idempotency_key | 各 service |

### 2.2 待落地

| 项 | 优先级 | 备注 |
| --- | --- | --- |
| Per-merchant bulkhead | 🟡 P2 | 单租户雪崩防护，需 Redis token bucket |
| Adapter timeout 标准化 | 🟢 已有但默认值散 | 统一到 5/15/30s 三档 |
| Chaos / soak test | 🟡 INFRA | toxiproxy + k6 |
| DB query timeout 全审计 | 🟡 P2 | grep 检查 ctx 是否传到 GORM |

---

## 三、可维护性（Maintainability）

### 3.1 已落地

| 项 | 实现 |
| --- | --- |
| 全栈 trace_id | payment-util/trace + OTel exporter；fx UnaryServerInterceptor 自动注入 |
| structured logging | zap 全栈，trace_id + bg_task 自动字段 |
| fx DI | 11 服务一致 fx provide/invoke 装配 |
| etcd 服务发现 | DialWithFallback：endpoints 非空 → etcd resolver；空 → 静态 endpoint 回退 |
| Shadow 流量隔离 | TableName / RedisKey / KafkaTopic 全栈 ctx 路由；不漏写主表 |
| Dockerfile sibling staging | 11 个服务一模板：本地兄弟仓优先，缺失 → git clone with token |
| init SQL 自动灌库 | bootstrap.sh + db-init container 启动时全 shard 灌完才放后续服务起 |
| assertProdSafety 全栈 | 11 服务每个都有，prod env=prod 时强制必填项 fail-fast |
| 文档化 | docs/ 14+ 篇：审计 / runbook / 设计 / 改造盘点 |
| Monorepo ↔ child repo 双向同步 | tools/sync_repos.sh，避免子仓 drift |
| Adapter unit test | card-payment/visa_test.go BIN 决策表（template，其他 4 家 follow） |

### 3.2 待落地

| 项 | 优先级 | 备注 |
| --- | --- | --- |
| 其余 4 家 adapter 单测 | 🟢 P3 | mc/jcb/amex/unionpay 复制 visa_test.go 模板 |
| End-to-end smoke test | 🟡 P2 | 跑通 注册 → 绑卡 → 支付 → 退款 → 对账 |
| 服务依赖图 / sequence diagram | 🟡 nice-to-have | mermaid in /docs |
| Error code table 标准化 | 🟡 P2 | gRPC status.Code → 业务错误 code |
| BIN routing 自动选 network | 🟢 P3 | processor.detectNetworkFromPAN 已有，但 caller 没用 |

---

## 四、跨服务一致性 checklist

启动期 fail-fast 项目（11 服务全部对齐）：

```
✅ env=prod 强制 mTLS cert/key/ca 三件齐全
✅ env=prod 拒绝 auth bypass / dev secret
✅ env=prod 拒绝 mockserver / localhost endpoint
✅ env=prod 强制 RS256 (user-merchant-core JWT)
✅ env=prod 强制 audit kafka_brokers 配置
✅ env=prod 强制 KMS RequireAndVerifyClientCert
✅ env=prod 拒绝 reconcile/CB/risk policy disable
✅ env=prod 拒绝 insecure_sandbox=true
✅ env=prod card-payment 拒绝 endpoint 指 .local/.internal/127.x
```

运行期监控指标（每服务 Prometheus 露出）：

```
✅ /healthz + /readyz K8s probe
✅ db pool open/idle/wait
✅ grpc request total/duration by method+code
✅ outbox lag (where applicable)
✅ circuit state (where applicable)
✅ business counters (charge_total / refund_total / authorize_total ...)
```

---

## 五、最近 24h 提交速览

| commit | 主题 |
| --- | --- |
| 472547fc | card-payment 熔断器 + 增强指标 + adapter 单测 |
| 138d5d6d | mock-network 完整化（Dockerfile + compose + 异步 webhook + 故障注入） |
| 558b4980 | 5 个 network adapter 真实卡组织协议 + 风险信号 |
| 689c9c69 | docs: SYSTEM_AUDIT_2026Q2 第 11 节收尾状态 |
| ee6ff0e6 | 清 dead PAN code + JWT kid rotation 多 key 验签 |
| bde71cae | 解 SQL/Dockerfile/HTML/go.mod 余下 25 个 conflict 标记 |
| d7dda447 | 解 21 个文件未解决 conflict 标记 |
| a636d29b | card-payment cardcenterclient 用 cardcenterv1.CardCenterClient |
| 82f4b5d3 | ctx batch 4: user-merchant retention + 文档收口 |
| 28c81383 | ctx batch 3: 4 个 worker (accounting + order-core outbox) |
| 17fee3e5 | ctx batch 2: order-core + accounting-system worker 全量 NewBackground |
| 5af475b9 | ctx batch 1: 渠道回调 + 关键 worker trace_id + shadow=false |
| a212df89 | card-center wire kafka-go audit producer (P1-7) |
| 896985ab | payment-util: trace.NewBackground + audit/kafkago |
| a6c2f7ba | kms-manage server mTLS + Client Identity allowlist (P0-4) |
| 2e661027 | card-payment reconcile worker (P0-2 资金安全) |
| fc4f106a | card-center audit prod fail-fast + card-payment metrics |
| 24265d00 | card-center prod safety + rate limit + KMS timeout |
| 8f334f99 | wire real card-center Detokenize RPC (P0-1) |

---

## 六、上线 readiness 评分

| 维度 | 评分 | 备注 |
| --- | --- | --- |
| 资金安全 | A | 双记账 / 幂等 / 单跳 PAN / Reconcile / HARD-decline 黑名单候选 |
| 身份鉴权 | A | mTLS 全栈 + JWT RS256 + KMS 三层 |
| 数据隔离 | A | shadow / shard / mch_id WHERE 强制 / RejectClaimedUserID |
| PCI 卡数据 | A- | SAQ-D 代码合规；待 ASV scan |
| 可用性 | B+ | 熔断 + bulkhead 部分；soak 未做 |
| 可观测性 | B+ | metrics + trace + audit_log；alerting 还没全面接 |
| 部署 / 运维 | B+ | dev 一键起；prod runbook 还差独立 DC 网络 |
| 技术债 | B | 8 个 P2 / P3 长期项排期，有 doc 跟踪 |

**结论**：核心代码门槛已通过。剩下都是外部依赖（ASV / DC / chaos 演练）+ 长尾 P3 优化。
