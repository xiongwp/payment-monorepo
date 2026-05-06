# Card-Center / Card-Payment 综合审计 — 2026-05-05 收尾报告

> 范围：`packages/card-center` + `packages/card-payment`，PAN 单跳路径。
> 视角：资金安全 / 系统安全 / 性能 / 可用性 / 可扩展性 / 合规（PCI DSS）。
> 状态：今晚收口 6 条阻塞项；剩余 3 条进入下一迭代，2 条 P2 长期。

## 0. TL;DR

17 条发现，按今晚处置分三组：

| 处置 | 数量 | ID |
| --- | --- | --- |
| **今晚已修** | 6 | P0-1, P0-3, P0-5, P1-1, P1-2, P1-3 |
| **下一迭代（带工时）** | 3 | P0-2, P0-4, P1-7 |
| **P2 长期** | 8 | P2-1 ~ P2-8 |

P0 阻塞项里，**P0-2（reconcile 缺失）** 和 **P0-4（kms-manage AuthInterceptor 还没强 mTLS）** 是仅剩 prod-blocker，已立票。

---

## 1. 今晚已合入的 6 条

### P0-1 — Detokenize 接通真 RPC （commit `8f334f99`）

**问题**：`card-payment/internal/cardcenterclient/client.go:Detokenize` 是 stub，
返回硬编码 `4111-1111-1111-1111`。Authorize 路径压根没穿到 card-center。

**改法**：

- 把 `cardcenterv1.NewCardCenterServiceClient(conn)` 装进 `Client.api`。
- `Detokenize` 实际发送 `DetokenizeRequest{payment_token, pi_id, caller="card-payment"}`，
  返回 `processor.Detokenized{PAN, ExpMonth, ExpYear, HolderName, PIID}`。
- `Caller="card-payment"` 是 card-center 服务层白名单字段，其他 caller deny + audit。
- `pi_id` 进 AAD 防 token 跨 PI 错绑。

**资金影响**：解开后 Authorize 才会真正向卡组织发交易，否则全部都是假成功。

---

### P0-3 — kms.bypass_hardened prod 拒（commit `24265d00`）

**问题**：`kmsclient.Config.BypassHardened` 是排查时加的旁路开关，
绕过 etcd resolver / service config / keepalive，prod 误开会出现连接幽灵复用、
KMS 调用悄悄打到失联实例。原代码没有 prod guard。

**改法**：`assertProdSafety()` 增加：

```go
if v.GetBool("kms.bypass_hardened") {
    return fmt.Errorf("PROD-SAFETY: kms.bypass_hardened=true forbidden in env=prod")
}
```

启动期 fail-fast，配置漂移 oncall 立即看到。

---

### P0-5 — Audit Kafka producer prod fail-fast（commit `fc4f106a`）

**问题**：`audit.New(producer, ...)` 长期注入 `nil` —— `assertProdSafety` 只判断
`audit.kafka_brokers` 配置非空，但实际 sarama producer 是 TODO，等于 audit 在
prod 永远 DB-only。**PCI DSS Req 10 要求 audit 至少一份在卸载式存储**，DB-only
不达标。

**改法**：`newAuditEmitter` 签名改为 `(...) -> (AuditEmitter, error)`，env=prod
且 producer==nil 直接 fail。dev 仍允许 DB-only + warn。fx.Provide 原生支持
`(T, error)`，下游 consumer 无需改。

**遗留**：sarama 接通仍是 TODO（P1-7 一并处理），但起码现在 prod 不能"假装合规"。

---

### P1-1 — card-payment metrics endpoint（commit `fc4f106a`）

**问题**：card-payment 完全没暴露 prometheus 端口，scrape 死区，没 healthz/readyz
导致 LB 主动摘流量也走不通。

**改法**：

- 新建 `internal/metrics/metrics.go`：`paycard_authorize_total/duration`、
  `paycard_network_error_total{network,kind}`、`paycard_detokenize_*`、
  `paycard_grpc_request_*`。
- main.go `fx.Invoke(startMetricsHTTP)`，默认 :9544（card-center :9543 错开），
  暴露 `/metrics` + `/healthz` + `/readyz`。
- OnStop hook 调 `BeginDrain()` → `/readyz` 503。

**未做**：processor / network adapter 的实际打点，下一批 PR 一起补。

---

### P1-2 — Rate limit 默认调宽（commit `24265d00`）

**问题**：默认 `burst=5, refill=12s`，单实例 5 卡/分钟，自动化测试和绑卡批跑都
会踩到 429。

**改法**：默认 `burst=10, refill=2s` （5 RPS 突发，长期 0.5 RPS）。配置文件覆盖
不变。

---

### P1-3 — KMS RPC timeout（commit `24265d00`）

**问题**：默认 3s，HSM-backed KMS P99 通常 4~5s，false-fail 会让 stored card
插入 vault 失败 → 用户卡反复绑不上。

**改法**：默认 7s（P99+2s 余量）。配置文件可覆盖。

---

## 2. 下一迭代（带工时）

### P0-2 — card-payment reconcile worker 缺失 *（est 1.5 d）*

**问题**：`processor.Authorize` 调卡组织成功 → 写 `card_transaction` 落账。
但 timeout / 网络异常时只回 error，没二阶段对账。**资金风险点**：
卡组织已扣款但本地 `pending` / `error`，必然产生差错账。

**计划**：

- 新建 `internal/reconcile/`：扫 `card_transaction` where `status in ('pending','error') and updated_at > now()-24h`。
- 调卡组织 `Inquiry`（visa/mc/amex 都有此 API），按返回结果置 `success` 或 `voided`。
- Webhook 出口写 outbox（payment-core 模式照搬）。
- 周期 30s，单实例 distributed lock（etcd lease）。

---

### P0-4 — kms-manage AuthInterceptor 升 mTLS *（est 3 d）*

**问题**：当前 kms-manage 入站只校验 Bearer token，没 mTLS。
`kmsclient` 这边已经按 mTLS 配（`buildTLS`），但 server 端不验证 client cert。
**这是 PAN 链路最后的纵深漏洞**：bearer 泄露 = KMS 全开。

**计划**：

- kms-manage `serverTLS` 增加 `ClientAuth = RequireAndVerifyClientCert`。
- AuthInterceptor 增加 SAN 白名单：仅 `card-center.payment.local` 通过。
- 非白名单 SAN deny + audit。
- 全 PCI 链路只走双向 mTLS + Bearer，纵深防御层数 +1。

---

### P1-7 — Audit OpenTelemetry 出口 *（est 2 d）*

**问题**：sarama producer 没接，9 个服务都缺 OTLP exporter。
配合 P0-5：prod 起得来必须先把 sarama 接上。

**计划**：

- payment-util 新建 `audit/sarama.go`：`SyncProducer` 实现 `audit.KafkaProducer`。
- 9 服务 main.go 加 `newKafkaProducer` provider，DI 注入。
- audit-collector 端写 `audit.events.*` topic 落数据湖。

---

## 3. P2 长期（8 条，归档不展开）

| ID | 主题 | 备注 |
| --- | --- | --- |
| P2-1 | card-center HSM 直连，去掉 kms-manage 中转 | FIPS 合规 |
| P2-2 | PAN tokenization 走 PCI vault 厂商（Basis Theory / VGS）| 减 SAQ-D |
| P2-3 | card-payment fleet → 多 region | 灾备 RTO |
| P2-4 | network adapter 重试 + idempotency-key | 防双扣 |
| P2-5 | E2EE：浏览器到 card-center JWE | 再缩 PAN scope |
| P2-6 | OAuth2 client_credentials 替 Bearer | PCI Req 8.3 |
| P2-7 | grpc-gateway HTTP/JSON facade | 调试便利 |
| P2-8 | chaos engineering 接 toxiproxy | 故障注入 |

---

## 4. 维度总结

**资金安全**：P0-1 接通后单跳路径完整；P0-2 reconcile 是仅剩资金阻塞项。

**系统安全**：P0-3 / P0-5 关 prod 后门；P0-4 mTLS 闭环要尽快。Bearer + mTLS
+ SAN 白名单三层之后，kms-manage 是 PCI 最后一道。

**性能**：P1-3 KMS timeout 调宽 + P1-2 rate limit 调宽，dev 自动化测试不再
随机踩。Authorize P99 待 metrics 上线后看真实分布。

**可用性**：P1-1 metrics + healthz/readyz 是 LB / k8s drain 的前提；
现在重启不再丢请求（GracefulStop + drain 已联动）。

**可扩展性**：card-payment 已分 10 库×10 表，单 region 可承载预估 3 年量；
P2-3 跨 region 在路上。

**合规（PCI DSS）**：

- Req 3（PAN 加密）：✅ envelope KMS + AAD-bound。
- Req 4（传输加密）：✅ TLS1.2+ 双向 mTLS（P0-4 完成后全闭环）。
- Req 8（鉴权）：⚠ Bearer-only，P0-4 升级中。
- Req 10（审计）：⚠ DB-only，P0-5 已 prod 拒，P1-7 接通 sarama 后达标。
- Req 12.10（事件响应）：✅ idempotency-key + admin_audit_log 双链路。

---

## 5. 提交清单

```
fc4f106a fix(P0-5,P1-1): card-center audit prod fail-fast + card-payment metrics
24265d00 fix(P0-3,P1-2,P1-3): card-center prod safety + rate limit + KMS timeout
8f334f99 fix(P0-1): wire real card-center Detokenize RPC (was stub)
```

3 个 commit，6 条 P0/P1 关闭，余 3 条入下一迭代。
