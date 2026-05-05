# 支付系统全面审计报告 — 2026Q2

**评估范围**：10+ 服务（api-gateway / user-merchant-core / order-core / payment-channel / payment-core / accounting-system / risk-manage / kms-manage / card-center / card-payment）+ 基础设施（11×MySQL / Redis Sentinel / Kafka / NebulaGraph / KMS）+ 部署（docker-compose stack + k8s 模板）。

**评估方法**：四维度交叉扫描（资金安全 / 身份鉴权 / 数据隔离 / PCI），每条发现给 file:line + 严重度 + 修复方向。本报告同时整合了过去 6 个月已完成 60+ 项修复（task #1~#83）的现状与剩余风险。

---

## 一、整体评估（Top-line）

| 维度 | 当前评级 | 主要剩余风险 |
|---|---|---|
| 资金安全 | **B+** | 异步双记账缺 RequestID；refund 创建无幂等键 |
| 身份鉴权 | **B** | JWT 仍是 HS256；OTP challenge query string 可被钓鱼 |
| 数据隔离 / 越权 | **A-** | shadow / shard 已 P0 修过；少数僵尸 PAN field 待清 |
| PCI 卡数据 | **A-** | PAN 单跳已落地；user-merchant-core 还有 dead PAN 字段定义 |
| 可用性 / 稳定性 | **B+** | dev 默认配置友好；prod fail-fast guard 已就位 |
| 可观测性 | **B** | trace + metrics 全栈到位；audit_log 链式签名 |
| 部署 / 运维 | **B** | dev 一键起；prod 需独立 DC for SAQ-D |
| 技术债 | **C+** | 多个 stub 客户端未接通真实 gRPC；UserCardService proto 待生成 |

**整体可上线判断**：核心资金链路已具备生产基线（双记账 / 幂等 / outbox / risk CB / shadow 隔离），P0 漏洞清空。**剩余阻塞项 6 条**（详见第七节），完成后可以小流量灰度。

---

## 二、资金安全（Funds Safety）

### 🔴 Critical: 异步 GL 入账缺 RequestID 幂等键
- **文件**：`packages/accounting-system/internal/service/hybrid_accounting_service.go:449`
- **现象**：`HybridDoubleEntryBooking` 调 `AsyncRecordEntry` 时未传 `RequestID`，但下游 Kafka 消费者要靠 RequestID 去重
- **风险**：Kafka 重试 + Ledger consumer 重复消费 → GL 双记 → 试算余额异常
- **修复**：`RequestID: fmt.Sprintf("%s:ledger_entries", voucherNo)` 注入确定性键

### 🟠 High: AsyncRecordEntry 没校验 LedgerEntries debit==credit
- **文件**：`packages/accounting-system/internal/service/hybrid_accounting_service.go:362-459`
- **现象**：order-core 那侧的 `PostingRequest.Validate()` 强制 debit==credit；但 accounting-system 接收侧没 guard
- **风险**：异常或恶意客户端发不平衡分录 → GL trial balance 长期偏差
- **修复**：入队前补一次 `req.ValidateLedgerBalance()`

### 🟠 High: Webhook claim_token 极小冲撞窗口
- **文件**：`packages/order-core/internal/webhook/delivery.go:237-270`
- **现象**：纯 16-byte random + 时戳生成 claim_token，多 worker 并发理论存在碰撞
- **修复**：改成 `claim:<worker_id>:<unix_nanos>` 元组，用 hostname / pod_name 做 worker_id

### 🟠 High: Refund 创建无幂等键
- **文件**：`packages/order-core/internal/service/services.go:991-1121`
- **现象**：`refundService.Create()` 不接受 `idempotency_key`，只在 `MarkSucceeded` CAS 阶段防双扣
- **风险**：客户端 retry 创建 → 两条 refund 行；CAS 防止双扣但商户看到重复退款记录
- **修复**：加 `IdempotencyKey` 入参，`(mch_id, idempotency_key)` 唯一索引

### ✅ Clean
- order-core `PostingRequest.Validate()` 强制双记账平衡（services.go ledger 部分）
- refundService.MarkSucceeded 用 CAS 严格防双扣
- webhook (channel_name, event_id) 复合唯一防双投递

---

## 三、身份鉴权（AuthN/AuthZ）

### 🟠 High: JWT 用 HS256（生产应换 RS256）
- **文件**：`packages/user-merchant-core/internal/authpkg/auth.go:1-6`
- **现象**：注释里写 "生产应换 RS256 + KMS-backed key" 但目前还是 HS256 共享密钥
- **风险**：密钥泄漏 = token 伪造；env=prod 没 guard
- **修复**：(1) 实现 RS256 私钥 + KMS 签名 (2) `assertProdSafety` 加 `auth.alg == "RS256"` 强制

### 🟡 Medium: OTP challenge 接受 query string
- **文件**：`packages/api-gateway/internal/userweb/handler.go:165-169`
- **现象**：`r.URL.Query().Get("challenge")` 作 cookie 兜底
- **风险**：钓鱼链接 `?challenge=xxx` → 会话固定攻击
- **修复**：删 query 接受，仅 cookie

### 🟡 Medium: OTP cookie path 太宽
- **文件**：`packages/api-gateway/internal/userweb/handler.go:104-109, 146-151`
- **现象**：`Path="/"` 任何端点都能读
- **修复**：`Path="/verify-otp"` 收窄

### ✅ Clean / Strong
- mTLS clientCN allowlist 默认拒（card-center/server/interceptor.go:75-100）
- `assertProdSafety` 在所有 8 服务启动期 fail-fast（auth/KMS/限流/TLS）
- IntrospectToken 同时校验 JWT 签名 + user_sessions 表，登出可即时撤销
- JWT.ParseWithClaims 显式拒 `alg=none` / 非 HS256
- Cookie 已设 HttpOnly / Secure / SameSite=Lax

---

## 四、数据隔离 / 越权（IDOR & Multi-tenancy）

### 🟢 Low: Dead PAN field in user-merchant-core
- **文件**：`packages/user-merchant-core/internal/cardcenterclient/client.go:40`
- **现象**：`TokenizeRequest.PAN string` 还在；但 `user_card_service.go::AttachCard` 已不再调 Tokenize（PAN 单跳后）
- **风险**：低（dead code），但留着诱使后人重新写 PAN 路径
- **修复**：删 `TokenizeRequest` 整个类型 + cardcenterclient.Tokenize 方法

### 🟢 Low: order-core cardcenterclient PAN 不再用
- **文件**：`packages/order-core/internal/cardcenterclient/client.go`
- **现象**：仅保留 MaskedPAN 引用；PAN 字段已清。但整个 client 包内大段 dead code
- **修复**：跟 service.card_payment.go 一起整理

### ✅ Strong
- card-center 的 stored_token / payment_token 都按 user_id / pi_id 分片，跨用户物理隔离
- shadow 流量已经 P0 全量改造（task #43-#48）：所有 repo 用 `shadow.TableName(ctx, base)` 路由
- mch_id 在 order-core 所有 repo 的 WHERE 子句里强制（task #21、P0-1 已修）
- card-center HTTPS handler 严格 3 道防线（middleware.go header）：jwt → IntrospectToken → user_id 只从 ctx
- `RejectClaimedUserID` 严格拒绝任何 body user_id（不只是不匹配）

---

## 五、PCI 卡数据（CHD Lifecycle）

PAN 单跳化（task #81）已完成。当前流向严格收紧到：

```
浏览器 ──HTTPS(PAN)──> card-center ──KMS encrypt──> stored_token ──> 浏览器
                          │
                          └─ 支付时 mTLS gRPC 给 card-payment ──> 卡组织
```

**保证**：api-gateway / user-merchant-core / order-core / payment-channel **进程内永远不见 PAN**。

### ✅ 已落地的 PCI 纪律
- card-center HTTPS handler 从 ctx 取 user_id；body user_id 一律 403
- `RejectClaimedUserID`：哪怕匹配也拒，强制客户端按规约写
- masked_pan 入 audit_log + payment_token_used（取证显示）
- Detokenize 严格 INSERT-or-fail 一次性约束（先 MarkUsed 再 Decrypt，replay 防御）
- AAD 绑定：stored_token → user_id，payment_token → pi_id，跨绑定立即 ErrAADMismatch
- HTTPS 入口套 HSTS + X-Frame-Options:DENY + Cache-Control:no-store
- per-user_id rate limit on tokenize（5/分钟，防 carding）
- PAN 在 vault 内只出现在解密返回的 `Detokenized` 结构；调用方 defer 清栈

### 🟡 剩余项
- user-merchant-core proto 改 AttachCard 后 stub 还没 `make proto` 重生
- card-center 当前与其它服务共 MySQL（dev 简化）；prod 必须独立 DC 才合规 SAQ-D
- ASV 扫描 + pentest + SAQ-D 自评估 还未走

---

## 六、可用性 / 稳定性

### ✅ 已落地
- 全栈 trace_id 注入 + OTel 接入
- DB 连接池上限 + Prometheus 监控（task #8）
- risk-manage 熔断器 + fail-policy 显式（task #44, #64）
- gRPC keepalive + KeepaliveEnforcementPolicy（防 too_many_pings GOAWAY）
- 优雅关停：BeginDrain → readiness 503 → 留 5s 让 K8s 摘流量 → 真停 server
- outbox lag Prometheus 指标（task #67）
- 限流：api-gateway per-IP RPS / per-merchant RPS；card-center per-user tokenize bucket

### 🟡 剩余技术债（非阻塞）
- 多实例部署时 card-center 内存 rate limit 不共享 → 需要 Redis 替换（接口已抽）
- 数据湖 CDC pipeline 设计完成但未上线（task #71）
- A/B 账户体系停在 60 / 61（用户主动叫停，风险高暂搁）

---

## 七、阻塞上线的 P0 / P1 项（6 条）

| # | 严重度 | 描述 | 文件 / 任务 | 工作量 |
|---|---|---|---|---|
| 1 | P0 | accounting AsyncRecordEntry 加 RequestID | hybrid_accounting_service.go:449 | 30min |
| 2 | P0 | accounting LedgerEntries 收侧加 balance 校验 | hybrid_accounting_service.go:441 | 1h |
| 3 | P0 | order-core refund Create 加 idempotency_key | services.go:991 + DB UNIQUE 索引 | 2h |
| 4 | P0 | JWT 改 RS256 + KMS 私钥 + assertProdSafety guard | authpkg/auth.go | 1d |
| 5 | P1 | webhook claim_token 改 worker_id+nanos | delivery.go:237 | 30min |
| 6 | P1 | OTP challenge 删 query 接受 + cookie 收窄 path | userweb/handler.go:165, 104, 146 | 1h |

**预估总工作量**：~2.5 天单人；可并行到 1 天。

---

## 八、技术债（不阻塞但建议下个迭代）

1. user-merchant-core UserCardService gRPC stub 接通（task #75，proto 已写好）
2. user-merchant-core / order-core 的 cardcenterclient 整理：删 dead PAN 字段
3. card-center 多实例 rate limit Redis 化
4. card-center / card-payment 独立 DC + 独立 KMS（kms-card）部署
5. PCI ASV scan + pentest + SAQ-D 自评估
6. 数据湖 CDC pipeline 上线
7. A/B 账户体系（按用户决定是否做）
8. 全链路 chaos / soak test
9. JWT key rotation 流程 + 旧 kid 验签兼容窗口
10. card-payment 5 个网络 adapter 接通真实卡组织 endpoint

---

## 九、合规视角（Compliance）

| 要求 | 状态 | 备注 |
|---|---|---|
| PCI-DSS SAQ-A-EP（非 PAN 服务） | ✅ | api-gateway / user-merchant-core / order-core 进程不见 PAN |
| PCI-DSS SAQ-D（card-center / card-payment） | 🟡 | 代码合规；需独立 DC + ASV scan |
| PCI-DSS 10.7（审计 7 年） | ✅ | audit_log + 链式 sha256 防篡改 |
| GDPR-style data-subject delete | 🟡 | 设计中，未实现端到端 |
| 反洗钱 (AML) 监控 | 🟡 | risk-manage 在做行为图谱，但合规规则待对接 |

---

## 十、生产上线前 checklist

- [ ] P0 1-4 项修完
- [ ] env=prod 跑通端到端冒烟（绑卡 → 支付 → 退款 → 对账）
- [ ] 灰度 1% 流量，盯 Prometheus 看 outbox_lag / refund_success_rate / payment_p99
- [ ] 灾备演练：kill 主 MySQL 1 个 shard，看 readiness 切流
- [ ] PCI 独立 DC 网络隔离（FW 规则 + VPC peering）
- [ ] mTLS cert 自动轮换（cert-manager）
- [ ] 备份 / restore 演练 1 次
- [ ] 安全扫描通过（gosec / trivy / ASV）
- [ ] runbook 给到 SRE / 客服

---

**报告结尾**。本审计基于静态代码 + 历史修复记录；动态测试 / 渗透测试 / 真实流量 chaos 还未做。
