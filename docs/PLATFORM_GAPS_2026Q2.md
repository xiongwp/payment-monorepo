# 支付平台功能差距评估 — 2026 Q2

> 范围: **产品 / 业务 / 合规 / 数据 / 资金正确性**
> 不含: SRE / 基础设施 / 部署 / 监控告警 / CI 这类工程化议题 (有独立 SLO 评估文档)
>
> 现有包 33 个 — 平台广度足够, 这次评估**集中讲深度**。

---

## 一、得分总览

| # | 维度 | 评分 (0-3) | 现状 | 主要缺口 |
|---|---|:---:|---|---|
| 1 | 3DS / SCA / EMV | 2 | step-up challenge 已通; 业务方 verdict→挑战联调通 | 缺 frictionless exemption (低额白名单, TRA, recurring) + AReq/ARes 真实协议字段 |
| 2 | AML / 制裁筛查 | **1** | risk-manage 框架在; KYC 收件流程在 | **没有 OFAC SDN / EU 制裁名单 / PEP 库接入**; 跨境合规红线 |
| 3 | 网络代币化 (VTS/MDES) | **1** | card-center 有 PAN→token 内部映射 | **没接 Visa Token Service / Mastercard MDES**; recurring 卡保留长期持有 PAN 监管违规风险 |
| 4 | FX / 多币种 | 3 | fx-service 独立; ECB 源 + bid/ask/mid + 锁价 | 完整, 缺生产对冲账户 (treasury) 视角 |
| 5 | 退款 / 争议生命周期 | 2 | refund-engine + dispute-service 状态机完整 | 缺 pre-arb / 仲裁阶段; 缺 issuer 应答轮回; chargeback reason code mapping 不全 |
| 6 | API Idempotency | 2 | payout / charge 路径有 | refund/dispute/webhook 几条 API 未全 enforce; 没有 per-merchant TTL + body-hash 校验 |
| 7 | Outbox / Saga | 2 | retry_queue + Kafka 都铺了 | **没有 DB 持久化 outbox 表**; 服务死了消息会丢; 跨服务 saga 补偿路径不完整 |
| 8 | Merchant webhook | 1 | merchant-webhook 包在; HMAC 签名在 | 缺 sig version (v1 升级路径), 缺 timestamp 防重放窗口, 缺 ack idempotency, DLQ replay 路径粗糙 |
| 9 | 税务申报 | **0** | — | **完全没有**; US 1099-K / W-8 / W-9 / EU VAT / OSS / GB GST 全空 |
| 10 | PII / GDPR / RTBF | 1 | KMS 加密银行账号; util 有 redact 工具 | **没有 DSAR API**, 没有 RTBF 全链路删除; 数据保留策略 (retention) 没落地 |
| 11 | 出款 / 结算 | 2 | clearing-settlement 状态机 + reserve | ACH / SEPA / Wire / SWIFT 都是 stub; 没接真 bank rail; payout failure 重出款流程缺 |
| 12 | 银行对账 (bank reco) | 1 | reconplatform + trial_balance 在 | 没有 MT940 / CAMT.053 / CSV 银行流水入口; 银行 vs 内账三方对账没跑通 |
| 13 | 风控 / 反欺诈打分 | 1 | risk-manage 给 ALLOW/REVIEW/DENY | 没有 velocity 累积 (1h/24h/7d); 没有 device fingerprint; 没有 manual review queue; 没有 ML 评分 (在线 + 离线) |
| 14 | 订阅 / 周期账 | 2 | subscription 包; dunning + 退避在 | proration 半成品; smart retry (network decline code 分流) 简单; 缺 grace period + customer portal |
| 15 | 钱包 / 充值 / 余额 | 2 | wallet-service + 接入 accounting | KYC tiering 已分级; 缺 reload 失败资金保护 (pending hold); 缺 closed loop 跨币种钱包 |
| 16 | Money Flow Graph / 分账 | 2 | split-payment + editor + dry-run | 内存 repo, 没接持久化; 没生产 e2e 大额验证 |
| 17 | Sandbox / DX | 1 | mock server + test 卡 | **没有商户 test-mode 切换** (test_key vs live_key); 没生产 SDK (Node/Python/Java/Go); 没 webhook simulator |
| 18 | 商户入网 (KYB) | 1 | kyc-service + bank binding | 缺 micro-deposit 验证 (打两笔小额让商户确认); 缺 MCC 智能推荐; 缺自动核账 + 风险打分定价 |
| 19 | Audit 不可篡改 | 2 | sha256 hash chain | 缺 anchor (定期把 hash root 写入 immutable storage / S3 worm); 缺第三方 attestor |
| 20 | OpenAPI / SDK / DevPortal | 1 | gRPC proto 全; OpenAPI 落了几份 | 没生成 SDK; 没有开发者门户 (apiKey 管理 + log replay + 文档); 没有 Postman collection |

**评分含义**: 0 = 没有 / 1 = 框架在但 stub / 2 = 主路径通了缺补丁 / 3 = 生产就绪

---

## 二、按风险分层 — 必须立即做

### P0 — 合规红线 (没有就上不了线 / 罚款风险)

#### 1. 制裁筛查 / OFAC / PEP (现 1 分)
- 需求: 商户入网 + 大额出款 + 跨境支付都要过 OFAC SDN 名单 / EU consolidated / 联合国制裁
- 落地: 接 ComplyAdvantage / Refinitiv / Dow Jones / 自建 + 每日 SDN bulk 同步
- 涉及包: kyc-service (入网), clearing-settlement (出款), 新建 `aml-screening` 服务
- 工作量: ~3 周

#### 2. 网络代币化 (现 1 分)
- 需求: PCI-DSS 4.0 + Visa Mandate — 持卡商户 recurring 必须用 network token (NTI), 不能持有 PAN
- 落地: 接 Visa Token Service (VTS) + Mastercard MDES + Amex
- 涉及包: card-center (替换 token 实现), card-payment (推钱时用 NT), subscription (recurring 用 NT)
- 工作量: ~4-6 周 (VTS 接入有审计周期)

#### 3. 税务申报 (现 0 分)
- 需求: US 商户年收 $20k+ 要发 1099-K 给 IRS + 商户; EU VAT OSS; GB MTD
- 落地:
  - 新建 `tax-reporting` 服务收集年化金额
  - 接 Avalara / TaxJar / Vertex 计算
  - 1099-K e-file 通过 IRS FIRE
- 工作量: ~6 周 (有数据回溯)

#### 4. GDPR DSAR / RTBF (现 1 分)
- 需求: EU 用户有权要求导出 / 删除自己所有数据 (含交易记录脱敏)
- 落地:
  - 新建 `data-rights` 服务做工单 + 跨服务编排
  - 每个 service 暴露 `/internal/export?subject=` + `/internal/erase?subject=`
  - audit-log 要支持 tombstone (保留事件 hash, 内容 pii 字段抹掉)
- 工作量: ~4 周

---

### P1 — 收入正确性 / 商户体验 (有则更好, 缺则掉单)

#### 5. 银行 → 内账对账 (现 1 分)
- 现 trial_balance 只是内部账, 没跟银行入金 / 出金 reconcile
- 落地:
  - reconplatform 加 bank file source: SFTP + MT940/CAMT.053/CSV parser
  - 每日 T+1 三方 (gateway, channel, bank) 对账
  - 差异分到 reconcile/diff
- 工作量: ~2 周

#### 6. 风控 - velocity / 设备指纹 / ML 评分 (现 1 分)
- 落地:
  - velocity: redis sliding window — 卡号/IP/email/device 1h/24h/7d 累积
  - device fingerprint: 接 FingerprintJS / Sift / 自建 JS 上报
  - 在线打分: simple logistic / xgboost serving (TFServing)
  - manual review queue: biz-admin-web 加审单页
- 工作量: ~5 周

#### 7. Outbox 持久化 + Saga (现 2 分)
- 现 outbox 是内存的, 服务挂掉消息会丢
- 落地:
  - 加 `tx_outbox` 表 (per service): event_id, payload, status, retry_count
  - 同事务写 outbox + 业务行
  - 单独 publisher 扫表发 Kafka, 发完更新状态
  - 退款/扣款/换汇加 saga 补偿 (compensating tx)
- 工作量: ~3 周

#### 8. Merchant webhook 资料级可靠性 (现 1 分)
- 落地:
  - 加 `X-Webhook-Signature: v1=<hmac>` (版本化)
  - 加 `X-Webhook-Timestamp` + 5min 窗口防 replay
  - DLQ 表 + admin 一键 replay
  - delivery_attempts 表 (商户能看每次推送 status)
- 工作量: ~1.5 周

---

### P2 — 平台扩展性 (现在不阻塞, 半年内要做)

#### 9. Sandbox 双 key + SDK (现 1 分)
- test_pk_ / live_pk_ 前缀分; test 走 mock channel
- 生成 Node.js + Python + Java + Go + Ruby SDK (openapi-generator)
- 开发者门户: API key 管理 + 推送日志 + 模拟 webhook
- 工作量: ~4 周

#### 10. 真实 ACH / SEPA 出款 (现 2 分 — stub)
- 接 Stripe Treasury / Modern Treasury / Currencycloud
- 或直连: NACHA (US ACH), TIPS/SCT (SEPA), CHAPS/Faster Payments (UK)
- 工作量: 取决于走 PSP 还是直连, ~3-8 周

#### 11. 商户 micro-deposit 银行验证 (现 1 分)
- KYB 后向商户账户打 \$0.01 + \$0.02, 让商户回填确认
- 比单一 IBAN/routing+account 验证更可靠
- 工作量: ~1 周

#### 12. 3DS frictionless / exemption (现 2 分)
- TRA (Transaction Risk Analysis, EBA 名单)
- low-value (<€30) 免 SCA
- recurring MIT 免 SCA
- whitelist 商户 (cardholder 授权信任)
- 工作量: ~2 周

---

## 三、潜在隐藏问题 (审码扫到)

### 工程债

1. **id-generator vs leaf_alloc 双系统** — 部分服务用本地 ID 部分用集中分配, 长期要统一到 leaf_alloc (跟 user-merchant-core 模式)。
2. **配置中心 vs viper file 双源** — 不少服务还是直接读 yaml, 配置中心订阅没全打通; 灰度 / dynamic 改配置不一致。
3. **payment-channel adapter 没有 circuit breaker per-endpoint** — 单 channel 接口降级粒度太粗 (整个 channel 熔断), 应该 path-level。
4. **accounting-system 跨币种科目映射散在多处** — fx-service 出现后, presentment_amount / settlement_amount / fx_pnl 三栏没全 enforce, 部分流入手工记账。
5. **kms-manage 没有 KEK 轮换演练** — 加密了但 KEK 永远没换过, 一旦 KEK 泄露全数据库重新加密成本巨大。

### 业务漏洞

6. **没有"商户结算账户冻结"业务态** — risk hold 商户期间, payout 走应该全暂停, 当前没有显式 hold flag。
7. **clearing-settlement reserve 释放规则没写死** — rolling reserve / fixed reserve / risk reserve 三类没分; 当前一刀切。
8. **dispute 准备材料的 SLA timer 没接告警** — issuer 给 11 天回应, 当前没有"剩 N 天未提交"通知。
9. **subscription 失败重试没有 network decline code 路由** — Stripe / Adyen 都按"硬拒绝/软拒绝/重试可"分; 当前是简单线性退避。
10. **wallet 跨币种汇率冻结时间没设上限** — 商户拿到 quote 之后可以无限期凭 quote 下单, 应该 30s TTL。

---

## 四、推荐 60 天路线图

| 周 | P0 | P1 |
|:---:|:---:|:---:|
| 1-2 | AML 接入 (vendor 选型) | Outbox 表 schema + publisher |
| 3-4 | OFAC bulk + 入网 hook | 退款 / 出款 outbox 改造 |
| 5-6 | DSAR / RTBF 工单服务 | 银行 reco — MT940 parser + 日切 |
| 7-8 | 1099-K 数据汇集 | velocity + device fp + manual review |
| 9-10 | VTS / MDES 申请 + sandbox | webhook v1 sig + DLQ + replay |
| 11-12 | VTS 接入 + recurring 切换 | 微存款入网 + sandbox key 分类 |

---

## 五、要不要做的判断

| 不建议自己做 | 理由 |
|---|---|
| BIN 数据库维护 | 走 SkyMobile / Bin-IQ 商业源 |
| 合规咨询 | 找律所 + 注会 / 不要工程化 |
| 信用卡数据落库 (PCI Level 1) | 全走 tokenization vault, 自己不存 PAN |
| 真实 KYC 文档识别 | 走 Persona / Onfido / Trulioo |

---

## 六、汇总数字

- 现有包数: **33**
- 评分维度: **20**
- 总分: **30 / 60** (50%, 半成品平台)
- P0 红线项: **4** (AML/Token/Tax/GDPR)
- P1 收入项: **4**
- P2 扩展项: **4**
- 60 天工作量预估: **15-18 人月** (5-6 人团队 3 个月)
