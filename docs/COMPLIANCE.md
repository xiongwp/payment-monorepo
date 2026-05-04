# user-merchant-core — 上线合规对齐

本文按 **PCI-DSS v4.0** 和 **SOC 2 Type II** 两个常用标准，列出本服务当前
已落地的控制点、残留缺口、以及后续需要运营/法务一起签字的项。

> 范围：本服务 **只处理 merchant onboarding + KYC + channel credential
> 密文托管**。PAN（卡号）/ CVV / 完整 track data **永不进入本服务**，由
> payment-channel + kms-manage 隔离。因此本服务属于 PCI scope 中的
> "Connected system"（减轻等级，不是 CDE 本身），但仍需满足全部关键控制。

## PCI-DSS v4.0 映射

| Req | 要求 | 当前状态 | 落地点 |
| --- | --- | --- | --- |
| 2.2 | 仅安装/启用必要服务 | ✅ | `Dockerfile` 基于 alpine，仅 gRPC + /metrics 端口开放 |
| 2.3 | 管理访问必须加密 | 🟡 | 内部 mTLS 未启用（用户已确认内部可信，仅网关需要）；外部网关单独处理 |
| 3.2 | 敏感认证数据不得存储 | ✅ | 只存 `sha256(api_key)`，明文只在签发响应一次回传 |
| 3.5 | 密钥受保护；信封加密 | ✅ | `MerchantChannelSecret.ciphertext` 经 kms-manage 加密；KMS AAD = `merchant:ID:channel:CH:FIELD`；本服务不持有主密钥 |
| 3.6 | 密钥轮转 | ✅ | `RotateApiKey` API；rotate 后旧 key hash 被缓存失效；调用方 24h 内看到新旧均可识别 |
| 4.1 | 传输加密 | 🟡 | 内部 gRPC `insecure`（按用户决定）；外部网关层需 TLS 终止，待网关落地 |
| 6.2 | 第三方 / 自研代码漏洞管理 | 🟡 | `go.mod` 依赖锁定；建议接 `govulncheck` + `snyk` 到 CI |
| 6.4.3 | 管理界面鉴权 | 🟡 | bearer token / JWT 支持；mTLS 客户端证书方案待 infra |
| 7.1 | 最小权限访问 | ✅ | admin-web BFF 是唯一对外入口；本服务不暴露公网 |
| 8.3.6 | 凭据轮转策略 | 🟡 | API key 有 rotate；webhook_secret rotate 待加；KMS master key 轮转在 kms-manage 侧 |
| 10.2 | 审计日志 | ✅ | `admin_audit_log` + 链式 `row_hash`；`cmd/audit-verify` 检测篡改 |
| 10.3 | 日志包含 who/what/when/from | ✅ | audit interceptor 自动收 actor + ip + method + target + trace_id + duration |
| 10.5 | 日志防篡改 | ✅ | append-only 表 + `prev_hash` → `row_hash` 链；篡改会断链 |
| 10.7 | 保留 ≥ 1 年 / 立即可查 3 个月 | 🟡 | retention 默认 7 年，启动需配 `retention.enabled=true`；日志导出到中心化存储待落地 |
| 12.10 | 事件响应流程 | 📝 | 运行手册（runbook）待补；SLO/SLA + 告警规则待写 |

## SOC 2 Type II 对齐

| 类别 | Trust Service Criteria | 当前状态 | 备注 |
| --- | --- | --- | --- |
| CC6.1 | 逻辑访问 | ✅ | 仅内网 + bearer；外部经 admin-web BFF |
| CC6.6 | 访问密钥保护 | ✅ | KMS 信封加密，本地无主密钥 |
| CC6.7 | 传输保护 | 🟡 | 见 PCI 4.1 |
| CC7.1 | 威胁检测 | 🟡 | 限流 + 入参校验 + body size cap；WAF 在网关侧 |
| CC7.2 | 变更监控 | ✅ | `admin_audit_log` 链式签名 |
| CC8.1 | 变更管理 | 📝 | release flow / review 流程走 GitHub PR（已走）；合规审计记录单独表格 |
| A1.2 | 容量 / 可用性 | ✅ | `_db_pool_*` / `_grpc_request_duration` / OTel spans → SLO 告警 |
| PI1.1 | 处理完整性 | ✅ | 幂等键 + 事务 + 链式审计 |

## 残留缺口（按优先级）

1. **内外边界 mTLS**（PCI 4.1 / SOC CC6.7）：外部网关落地；本服务保留 insecure。
2. **日志中心化**（PCI 10.7）：zap → ELK / Loki；运维查询 TTL ≥ 1 年。
3. **告警规则 + runbook**（PCI 12.10）：Prometheus alert manager + Slack；SLO 定义（p99 < 200ms, error rate < 0.1%）。
4. **依赖漏洞扫描**（PCI 6.2）：CI 加 `govulncheck ./...`；重大 CVE 阻塞 merge。
5. **key rotation policy**（PCI 3.6）：webhook_secret 定期 rotate；KMS master key 年度轮转在 kms-manage。
6. **灾难恢复演练**：DB 主备切换 + cache 冷启动；每季度一次。
7. **数据分类标签**：哪些列是 PII / PCI scope 标在 DDL 注释里（已部分）。

## 已开具的控制证据（审计时要求提交）

- `admin_audit_log` 链式签名 —— `cmd/audit-verify` 出证
- `merchant_channel_secret.ciphertext` 非明文存储 —— schema 校验
- `merchants.live_key_hash` / `test_key_hash` 非明文 —— schema + code review
- Prometheus metrics 导出 —— `/metrics` 样本
- 依赖清单 —— `go.mod` + `go.sum`
- 变更记录 —— git log (`claude/setup-user-merchant-core-w0GCt` branch)
