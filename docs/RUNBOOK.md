# user-merchant-core 运维 runbook

每个章节对应一个 PrometheusRule 中的 `runbook` 链接。值班同学按"症状 → 检查 →
缓解 → 根因"四步走；不要绕过缓解直接做根因（先止血）。

## 概览

```
[admin-backend] ─grpc─> [user-merchant-core] ─sql─> [meta MySQL]
                                          │
                                          └─grpc─> [kms-manage]
```

服务无状态；缓存 in-process。重启 = 缓存丢失 + 重新 warmup。

## SLO

- 可用性：99.9%（每月停机 ≤ 43 分钟）
- p99 RPC 延迟：< 200ms（AuthenticateByAPIKey），< 500ms（其它）
- 错误率（非业务错）：< 0.1%

---

## high-error-rate

**症状**：`UserMerchantHighErrorRate` 触发；非 OK/NotFound/InvalidArgument/FailedPrecondition 占比 > 1%。

1. **看 Grafana / Error rate by code**：是 Internal / Unavailable / Unauthenticated 哪个？
   - Internal 多 → 大概率 DB 或 KMS 故障，看 `db_pool_*` 和 `kms_rpc_total`
   - Unavailable 多 → 上游 KMS / 下游网关
   - Unauthenticated 多 → admin-backend 配置错或 token 轮转

2. **缓解**：
   - 如果某个 method 集中爆 → 临时把它移出生产 traffic（admin-web feature flag / Envoy 黑名单）
   - 如果是单 pod 异常 → 扩 1 个 replica + drain 异常 pod

3. **根因**：去看 `kubectl logs --previous` 上一个 instance 的 stack trace；
   每条 grpc OUT 日志都带 trace_id，可在 OTel collector 里 join。

---

## p99-latency-high

**症状**：某 method p99 > 500ms 持续 10 分钟。

1. **看 OTel trace**：典型瓶颈
   - 在 `repo.Get` → DB 慢；查 `slow sql` 日志（zap gormlogger 自动打 > 200ms）
   - 在 `kms.Decrypt` → KMS 抖；看 `kms_rpc_duration` p99

2. **缓解**：
   - DB 慢 → 加 replica（ConfigMap `database.replicas`）让 List/Auth 走 RO；写还是主库
   - KMS 慢 → SecretCache TTL 临时调大到 30m（牺牲 rotate 时效换可用性）

3. **根因**：`EXPLAIN` 慢 SQL 看走索引；KMS 看 kms-manage 自己的 metrics。

---

## auth-cache-miss-spike

**症状**：`by_key_hash` miss > 30% 持续 10 分钟。

可能原因：
- 服务刚滚动重启，warmup 没覆盖到所有商户 → 等 60s 自然回升即可
- 大量 RotateApiKey 操作 → admin / 商户管理是不是出了 bug 在循环调？查 audit log
- 缓存太小 → `cache.merchant.size` 调大（推荐设到当前 active 商户数 × 2）

---

## db-pool-saturated

**症状**：`UserMerchantDBPoolSaturated` 或 `UserMerchantDBWaitGrowing`。

1. **缓解**：
   - 立即扩容 replica（HPA 不会基于 DB pool 自动扩，需要人为决定）
   - `kubectl set env deploy/user-merchant-core USERMERCHANTCORE_DATABASE_META_MAX_OPEN_CONNS=200`
     滚动重启（pool 配置不支持热加载）

2. **根因**：
   - 慢查询占住连接 —— `SHOW PROCESSLIST` 找
   - cache 命中率低 → 一切 DB 流量都走过去
   - HPA 没跟上业务 burst → 调 HPA `scaleUp` 更激进

---

## kms-down

**症状**：`UserMerchantKMSErrorRate` > 5% 或 `UserMerchantKMSP99Latency` > 1s。

1. **本服务影响**：
   - `Put / BulkGetPlaintext` 失败；payment-channel adapter init 拿不到密钥 → **新支付流量受阻**
   - 既有 SecretCache 还在 TTL 内的渠道继续工作；过期前可争取时间

2. **缓解**：
   - 把 SecretCache TTL 临时拉长到 30m / 1h（ConfigMap `cache.secret.ttl`）→ 滚动
   - 如果 KMS 完全不可用 → page kms-manage 值班

3. **回退**：
   - 不要 disable KMS！明文落库是 PCI 红线
   - 如果必须紧急上线某个商户而 KMS 挂着，临时让 payment-channel 走 mock secret（仅 sandbox 渠道）

---

## pod-not-ready

1. `kubectl describe pod <name>` 看 events
2. 常见原因：
   - **DSN 错** → ConfigMap/Secret 内容；`/healthz` 返 503
   - **KMS 拒绝** → bearer token 失效，看 startup log
   - **CrashLoop** → `kubectl logs --previous`

---

## 紧急快速操作

| 场景 | 命令 |
| --- | --- |
| 临时停止某 method | admin-web feature flag |
| 加 1 个 replica | `kubectl scale deploy user-merchant-core --replicas=N` |
| 强制清缓存（重启所有） | `kubectl rollout restart deploy user-merchant-core` |
| 拉 audit chain 完整性 | `audit-verify --dsn $DSN` |
| 看实时慢 SQL | `kubectl logs deploy/user-merchant-core | grep "slow sql"` |
| 查某 trace | OTel UI 搜 `trace_id=<xxx>`（admin-web error 页面会带） |

---

## 灾难恢复

1. **DB 全损**：从主备同步的 binlog / snapshot 恢复；recovery RTO 目标 ≤ 1h
2. **KMS 全损**：所有渠道凭据无法解密；触发 incident sev 1，联系 kms-manage 团队
3. **本服务镜像损坏 / GitHub 不可用**：
   - 拿任意 healthy pod `kubectl cp` 出二进制 + config
   - 临时 daemonset 起本地副本兜住流量
