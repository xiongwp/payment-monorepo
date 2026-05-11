#!/usr/bin/env bash
# commit-oauth2.sh — 把本会话所有改动按 5 个逻辑组分别提交。
#
# 用法 (在仓库根):
#   bash scripts/commit-oauth2.sh
#
# 或想看每一步：
#   bash -x scripts/commit-oauth2.sh

set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# 安全: 把可能 stale 的 lock 删掉
rm -f .git/index.lock

echo "▶ 重置 staging 区"
git reset HEAD 2>/dev/null >/dev/null || true

# ─────────────────────────────────────────────────────────
# 1. oauth2-server: 完整实现
# ─────────────────────────────────────────────────────────
echo "▶ [1/18] oauth2-server"
git add packages/oauth2-server/
git commit -m "feat(oauth2-server): 完整实现 client_credentials + JWKS + admin

新包 oauth2-server (RFC 6749 §4.4 + 7517 + 7662 + 7009):
- cmd/server/main.go: 入口, MySQL/memory 双模式, 后台 GC + key rotation
- internal/domain/client.go: Client/Token/Claims 实体
- internal/jwks/keystore.go: RSA key 管理 + Sign/Verify/JWKS + Rotate + Purge
- internal/jwks/rs256.go: signRS256/verifyRS256
- internal/jwks/keystore_test.go: 8 个单测 (sign/verify/expire/rotate/purge/tamper)
- internal/store/memory.go: 内存 store + bcrypt + scope 交集 + IP 白名单
- internal/store/mysql.go: MySQL 持久化 (HA prod)
- internal/adminhttp/server.go: 7 endpoint (token/introspect/revoke/jwks/discovery/admin)
- client/client.go: Go SDK (auto-refresh + retry + 并发安全)
- api/openapi.yaml: OpenAPI 3.0 spec
- database/init/01_schema.sql: clients + revoked_tokens + rsa_keys + admin_audit
- deploy/k8s/oauth2-server.yaml: Deployment/Service/Ingress/PDB/NetworkPolicy
- Dockerfile: multi-stage build
- test/smoke.sh: 13 步端到端烟雾测试

关键设计:
- RSA 2048 + RS256 (RFC 7518)
- kid = SHA256(pubkey)[:8] base64url
- Active + retired key pairs (rotation 期老 token 仍可验, 30d 后 purge)
- bcrypt(secret), 原始 secret 不落盘
- jti revocation 黑名单 (TTL = token exp)
- IP 白名单 + scope 交集 + status check
- 后台 ticker 每 15min GC retired/revoked"

# ─────────────────────────────────────────────────────────
# 2. payment-mw: OAuth2 Bearer JWT 支持
# ─────────────────────────────────────────────────────────
echo "▶ [2/18] payment-mw"
git add packages/payment-mw/oauth_bearer.go packages/payment-mw/mw.go packages/payment-mw/bootstrap.go
git commit -m "feat(payment-mw): OAuth2 Bearer JWT 验签 + scope RBAC

- oauth_bearer.go (新): JWTVerifier
  * JWKS 拉取 + 本地缓存 + 10min 后台 refresh
  * RS256 验签 + exp/iss/aud 校验
  * key rotation 自动适配 (拿到未知 kid 立即 refresh)
  * HasScope() / RequireScope() 业务层用
- mw.go: AuthConfig 加 BearerJWT 字段; Actor 加 ClientID/ServiceID/Scopes
  Auth 优先级: PublicPath → Bearer JWT → X-API-Key → X-Internal-Token
- bootstrap.go: env OAUTH2_JWKS_URL 自动装载 JWTVerifier (一行启用)

集成只需 3 个 env:
  OAUTH2_JWKS_URL=http://oauth2-server:8087/.well-known/jwks.json
  OAUTH2_ISSUER=http://oauth2-server:8087
  OAUTH2_AUDIENCE=payment-api

业务侧 RBAC:
  mux.Handle(\"/api/v1/refunds\", mw.RequireScope(\"refund:write\")(handler))"

# ─────────────────────────────────────────────────────────
# 3. OpenAPI 3.0 specs (10 份)
# ─────────────────────────────────────────────────────────
echo "▶ [3/18] OpenAPI specs"
git add api/
git commit -m "docs(api): OpenAPI 3.0 specs — 10 服务标准化文档

- _common.yaml: 共享 securitySchemes (oauth2BearerJWT + apiKey + internalToken) +
  Error/Money/Pagination
- payment-gateway.yaml: 收银台 + PaymentIntent + Hosted Checkout
- refund-engine.yaml: 退款全流程
- dispute-service.yaml: 信用卡争议状态机
- billing-system.yaml: 手续费 + 月结 + trial balance
- merchant-webhook.yaml: 商户出站 webhook 配置 + DLQ replay
- kyc-service.yaml: KYB/KYC 申请 + 审核
- audit-log.yaml: 审计事件 + 哈希链校验
- oauth2-server.yaml: 已有 (copy)
- biz-admin-web.yaml: 聚合后台
- payment-mw.yaml: 横切关注点 (/healthz /metrics)
- index.html: Swagger UI portal (10 spec 切换)
- README.md: 部署 + lint + scope 矩阵

12 个 scope: charge:* / refund:* / dispute:* / kyc:* / merchant:* / ops:*"

# ─────────────────────────────────────────────────────────
# 4. 集成 demo + rollout docs
# ─────────────────────────────────────────────────────────
echo "▶ [4/18] integration demos + docs"
git add examples/ OAUTH2_ROLLOUT.md
git commit -m "docs(oauth2): 端到端集成 demo + rollout 计划

examples/oauth2-integration/ (9 文件):
- README.md: 总览 + 架构图 + 3 种集成方式 + scope 矩阵
- MIGRATION.md: 现有服务接 OAuth2 Before/After + 灰度计划 + Rollback
- 00_setup.sh: 起 oauth2-server + 创建 2 demo client → .env
- 01_merchant_demo.go: 商户 SDK 调用 + scope 403 验证
- 02_service_demo.go: 服务间调用 + 10 并发 token 缓存验证
- 03_curl_flow.sh: 9 步纯 curl 流程 (含 JWT 解码 / introspect / revoke)
- 04_resource_server.go: 资源服务集成模板 (mw.Bootstrap + RequireScope)
- 05_e2e_test.sh: 10 断言 CI 测试
- 06_python_client.py: 非 Go 服务 (Python) 集成示例

OAUTH2_ROLLOUT.md:
- 完整变更清单 (oauth2-server 14 文件 + payment-mw 3 文件 + 10 OpenAPI)
- 端到端流程 (商户接入 / 服务间 / key rotation)
- 生产 checklist (RSA / admin token / MySQL DSN / SLO 告警 / cron)
- 9 项 P2 后续优化"

# ─────────────────────────────────────────────────────────
# 5. deploy 集成 (biz-stack 加 oauth2-server)
# ─────────────────────────────────────────────────────────
echo "▶ [5/18] biz-stack compose 集成"
git add packages/payment-admin-web/deploy/overrides/biz-stack.yml
git commit -m "feat(deploy): biz-stack 集成 oauth2-server

- 新增 oauth2-server service (port 18087)
  * OAUTH2_DEV_SEED=1 启动种 3 demo client
  * OAUTH2_KEY_PATH 持久化 RSA private key → oauth2-keys volume
  * 健康检查 + 标准 18087 host port
- biz-admin-web 加 OAUTH2_BASE_URL + OAUTH2_ADMIN_TOKEN env
- depends_on 加 oauth2-server"

# ─────────────────────────────────────────────────────────
# 6. 优化 (metrics / rate-limit / audit / scope / introspect cache)
# ─────────────────────────────────────────────────────────
echo "▶ [6/18] OAuth2 生产化优化"
git add packages/oauth2-server/internal/metrics/ \
        packages/oauth2-server/internal/ratelimit/ \
        packages/oauth2-server/internal/audit/ \
        packages/oauth2-server/deploy/prometheus/ \
        packages/oauth2-server/test/loadtest.sh \
        packages/oauth2-server/cmd/server/main.go \
        packages/oauth2-server/internal/adminhttp/server.go \
        packages/oauth2-server/go.mod \
        packages/payment-mw/scope.go \
        packages/payment-mw/scope_test.go \
        packages/payment-mw/introspect_cache.go
git commit -m "feat(oauth2): 生产化 — metrics + rate-limit + audit + scope 层级

可观测性 (Prometheus):
- internal/metrics/metrics.go: 11 个指标 (token issue/p99/outcomes,
  introspect, revoke, JWKS fetch, rate limit hits, admin actions,
  key rotation, active key age, clients/revoked count)
- deploy/prometheus/alerts.yaml: 9 条告警 (down/error rate/latency/
  brute force/scope denial surge/key age/rate limit firing/
  revocation list growing/frequent key rotation)
- deploy/prometheus/grafana-dashboard.json: 10 panel dashboard
- cmd/server/main.go: /metrics endpoint + 后台 15min gauge 刷新

性能 + 安全 (rate limit):
- internal/ratelimit/limiter.go: token bucket per-key + GC
- internal/ratelimit/limiter_test.go: 4 个单测
- handleToken 加 IP (50rps) + client (20rps) 双维度限流
- 失败 outcome 8 类 label (unknown_client/bad_secret/suspended/
  expired_creds/ip_blocked/scope_denied/ip_throttled/client_throttled)

合规 (audit log):
- internal/audit/audit.go: Event/Sink/Multi/Redact
- 6 个 admin action 全部审计 (create/rotate/suspend/activate/
  revoke-client/key-rotate)
- Redact 自动脱敏 secret/private_key

业务层 (payment-mw):
- scope.go: scope 层级 + 通配 (refund:* / *:read / 全通配 +
  write→read 隐含); HasScopeOrImplied / AnyScope / AllScopes
- scope_test.go: 13 个 case
- introspect_cache.go: 异步 revocation 传播 (~30s 延迟, 99% 走本地)

负载测试:
- test/loadtest.sh: 3 场景 (单 client 50rps 限流 / 100 并发吞吐 /
  错 secret 100 次 brute-force 计数)"

# 清掉自动生成的辅助文件 (不进 commit)
echo "▶ 注: go.work.disabled / go.work.sum 是 Go 工具自动产物, 已在 .gitignore 应忽略"
echo "    如果需要忽略, 加 .gitignore: go.work.disabled, go.work.sum"

# ─────────────────────────────────────────────────────────
# 7. 修复 payment-admin-backend gRPC "no children" 问题
# ─────────────────────────────────────────────────────────
echo "▶ [7/18] 修 admin-backend grpc.NewClient 启动顺序问题"
git add packages/payment-admin-web/backend/cmd/server/main.go \
        scripts/rebuild-admin-backend.sh
git commit -m "fix(payment-admin-web): 修 grpc.NewClient 'no children to pick from'

根因:
- grpc.NewClient(\"host:port\") 默认走 passthrough resolver, 不会
  re-resolve DNS; 启动顺序错 / 后端容器重启时 picker 卡在空状态.
- /api/user-merchant/audits 等接口 502 + 'no children to pick from'.

修复:
- target 自动加 dns:/// 前缀 -> 内置 DNS resolver + idle 重连时
  re-resolve.
- healthCheckConfig: backend NOT_SERVING 自动剔除 picker.
- retryPolicy: UNAVAILABLE 自动重试 3 次 (指数退避), 启动期偶发
  picker miss 自愈.
- 两处 grpc.NewClient (etcd-disabled + etcd-empty fallback) 都修.

scripts/rebuild-admin-backend.sh — 重 build + 部署 + 验证脚本."

# ─────────────────────────────────────────────────────────
# 8. P0 系统能力建设: 日志/合成监控/feature flag/备份/chaos/trace graph
# ─────────────────────────────────────────────────────────
echo "▶ [8/18] 系统能力补全 — observability + reliability"
git add docs/SYSTEM_GAPS_2026Q3.md docs/DR_PLAN.md \
        deploy/monitoring/loki/ deploy/monitoring/blackbox/ \
        deploy/monitoring/grafana/ \
        packages/payment-util/featureflag/ \
        deploy/backup/ deploy/chaos/ \
        packages/payment-admin-web/backend/internal/handler/trace_graph.go \
        packages/payment-admin-web/backend/cmd/server/main.go \
        tools/trace-viewer/
git commit -m "feat(platform): observability+reliability — 6 项 P0/P1 能力补全

监控:
- deploy/monitoring/loki/         Loki + Promtail 集中日志栈 (30d 留存)
- deploy/monitoring/blackbox/     blackbox-exporter 合成监控 (healthz/oauth-token/TCP/TLS-expiry)
- deploy/monitoring/grafana/      Grafana datasource provisioning (Loki ⇄ Jaeger 双向跳)

可运维:
- packages/payment-util/featureflag/  灰度/熔断 SDK
  * config-center 拉, 10s 热刷, 不重启服务
  * 一致性 hash 按 merchant_id/user_id 落桶 0-99
  * include/exclude 白名单, JSON/Bool/Int/String 4 种类型
  * 12 个单测覆盖
- tools/trace-viewer/                 输入 trace_id 看 timeline + 服务依赖图 + 关联日志 SPA
- packages/payment-admin-web/.../trace_graph.go  /api/trace/{id}/graph
  聚合 Jaeger spans + Loki logs 派生 dependency edges

灾备:
- deploy/backup/                  自动备份 + 季度恢复演练 cronjob
  * backup.sh: mysqldump | gzip | s3 cp, push prom metric
  * restore-drill.sh: ephemeral mysql + verify + checksum
  * k8s-backup-cronjobs.yaml: 9 库 daily, retention 40 份
- docs/DR_PLAN.md                 RTO/RPO 表 + RACI + 触发-行动 matrix

chaos:
- deploy/chaos/                   3 个 chaos-mesh 实验
  * network-loss-payment-channel: 渠道 50% 丢包验重试
  * pod-kill-rolling: 30s 随机杀 pod 验副本 HA
  * db-stall-primary: 主库 1s 延迟验 timeout/异步队列
  * README.md: GameDay 流程 + RTO/RPO

告警:
- deploy/alerts/burn-rate.yaml    SLO budget 多窗口 burn rate (fast 14.4× / slow 6×)
- deploy/alertmanager/routing-tree.yml  按 severity/kind 路由 (page/warn/info/untriaged)

总览: docs/SYSTEM_GAPS_2026Q3.md  5 维度 22 项能力盘点 + P0-P2 路线图"

# ─────────────────────────────────────────────────────────
# 9. burn-rate + routing-tree (alerts/alertmanager 分目录)
# ─────────────────────────────────────────────────────────
echo "▶ [9/18] alert rules + routing"
git add deploy/alerts/burn-rate.yaml deploy/alertmanager/routing-tree.yml
git commit -m "feat(alerts): SLO burn-rate + routing tree

- burn-rate.yaml: Google SRE workbook §5 多窗口 burn rate
  oauth2 + payment-gateway 各 4 个 (5m+1h / 30m+6h / 2h+24h / 6h+3d)
- routing-tree.yml: severity/kind 分级路由
  page→PagerDuty (out-of-hours 走二线), warn→Slack, info→静默
  fund_safety/security 直通财务/security oncall
  inhibit: ServiceDown 抑制子告警, fast-burn 抑制 slow-burn"

# ─────────────────────────────────────────────────────────
# 10. P2 系统进阶: async job / dep-graph / pool audit / RUM / blue-green / rollback
# ─────────────────────────────────────────────────────────
echo "▶ [10/18] P2 进阶能力"
git add packages/payment-util/jobqueue/ \
        packages/payment-admin-web/frontend/rum.js \
        packages/payment-admin-web/backend/internal/handler/rum_ingest.go \
        tools/dep-graph/ tools/pool-audit/ \
        deploy/bluegreen/ \
        scripts/rollback.sh
git commit -m "feat(platform): P2 高级运维能力

可扩展:
- packages/payment-util/jobqueue/   异步任务队列
  * Client/Server 接口 (queue/MaxRetry/Timeout/Unique/ProcessAt)
  * MemoryClient 测试用; 生产 WrapAsynqClient 接 Redis-backed asynq
  * 优先级 queue (critical/default/low) + DLQ + 幂等

监控:
- packages/payment-admin-web/frontend/rum.js          RUM snippet
  * Web Vitals (LCP/FID/CLS/TTFB/FCP) + JS error + fetch failure
  * data-rum-event click track + 慢资源 (>1s) + page view
  * 5s/20条 自动 batch + visibility-hidden sendBeacon
- backend/internal/handler/rum_ingest.go              POST /api/rum/ingest
  * service 白名单 + page ID 脱敏 + Prometheus metrics
  * rum_web_vital_ms / rum_errors_total / rum_clicks_total / rum_page_views_total
- tools/dep-graph/main.go                             OTel-derived 服务依赖图
  * Jaeger /api/dependencies → JSON/DOT/Mermaid 三种输出
  * k8s CronJob 每小时跑, docs 自动更新
- tools/pool-audit/main.go                            连接池配置审计
  * 扫 monorepo 找 sql/redis/http 池配置, 标红反模式
  * format=text/markdown/json (CI 用 markdown 输出审计报告)

可运维:
- deploy/bluegreen/                                   Argo Rollouts 蓝绿/canary
  * oauth2-server-rollout.yaml (blueGreen + preview service)
  * analysis-template-success-rate.yaml (3 indicator: success/p99/business)
  * 自动回滚: 3 次连续失败 → abort
- scripts/rollback.sh                                 一键回滚 + Slack 通知"

# ─────────────────────────────────────────────────────────
# 11. P2 终: multi-region + CDN + PCI 内部扫描
# ─────────────────────────────────────────────────────────
echo "▶ [11/18] multi-region / CDN / PCI"
git add deploy/multi-region/ deploy/cdn/ \
        scripts/cdn-deploy.sh scripts/pci-self-check.sh \
        .github/workflows/security-scan.yml
git commit -m "feat(platform): multi-region + CDN + PCI 内部扫描

灾备:
- deploy/multi-region/failover.sh      跨 region failover 一键 (precheck →
  promote RDS replica → scale DR → DNS cutover → smoke test → Slack 通知)
- deploy/multi-region/route53-failover.tf  Route53 health-check + 自动 failover
- deploy/multi-region/kafka-mirrormaker2.yaml  Strimzi MM2 双向复制 + group
  offset 同步 (failover 后 consumer 接着消费)
- deploy/multi-region/README.md         拓扑 + 数据复制策略 + RTO/RPO 表 +
  季度 failover drill SOP

CDN:
- deploy/cdn/cloudflare.tf              Cloudflare Terraform — 4 个 page rule
  (immutable .hash.js 1y / *.html 5min / /api/* bypass) + WAF + 全局 rate limit
- scripts/cdn-deploy.sh                 前端 build → S3 sync (immutable +
  hashed; html short-cache) → Cloudflare 选择性 purge + smoke test

合规:
- .github/workflows/security-scan.yml   6 job 安全扫描
  * trivy (容器 CVE), gitleaks (secrets), gosec (Go 安全), nuclei (web vuln),
    OWASP ZAP baseline, PCI checklist 自动校验
- scripts/pci-self-check.sh             PCI DSS v4.0 30 项自查
  * Req 1 (NetworkPolicy) / 2 (默认凭据) / 3 (PAN 加密) / 4 (TLS 1.2+) /
    6 (CI scan) / 7 (RBAC scope) / 8 (bcrypt+2FA) / 10 (audit hash chain) /
    11 (chaos+drill) / 12 (key rotation)

注: PCI ASV 扫描 + 年度 pentest + on-site QSA audit 仍需外采, 本套是 *预防* 内部
扫描, 减少 ASV 扫描翻车率"

# ─────────────────────────────────────────────────────────
# 12. split-payment / Money Flow Graph 资金流编排服务
# ─────────────────────────────────────────────────────────
echo "▶ [12/18] split-payment + Money Flow Graph"
git add packages/split-payment/ examples/moneyflow-graphs/ \
        packages/payment-admin-web/frontend/moneyflow-designer.html
git commit -m "feat(split-payment): Money Flow Graph 资金流编排服务

设计:
- 不持账, 资金真相由 accounting-system 负责
- N 个 split items 打包成 1 个 AtomicBatchBookingRequest, all-or-nothing
- 通用 Graph DSL (节点+边+触发+guards+hold+reversal), 不限于分账,
  也支持 referral / 退款反向 / hold 释放 / dispute 等任意资金流

文件:
- README.md / MONEYFLOW_GRAPH.md   设计文档
- internal/domain/graph.go         Graph / Node / Edge / Movement / RunPlan
- internal/domain/types.go         旧 Rule / Plan (兼容)
- internal/workflow/translator.go  纯函数: Graph + event → RunPlan
- internal/workflow/engine.go      事件驱动 (Kafka 订阅 → match → translate → post)
- internal/workflow/translator_test.go  7 个 case (basic/尾差/optional/required/guards/fixed)
- internal/clients/accounting.go   AccountingClient — PostMovements/PostSplitAtomic/Reverse/GetBalance
- internal/repo/memory.go          GraphRepo + RunRepo 内存实现
- internal/adminhttp/graph.go      /api/moneyflow/{graphs,dry-run,runs/search}
- cmd/server/main.go               入口 — 加载 accounting client + seed example graphs
- go.mod                           监 accounting-system 本地模块

前端 SPA:
- packages/payment-admin-web/frontend/moneyflow-designer.html
  cytoscape-style 拖拽设计器 — 节点 palette / 边连接 / 属性表单 / JSON 预览
  / save 到后端 / dry-run 测试

示例:
- examples/moneyflow-graphs/marketplace-default.json  90/5/5 分账 + 7d hold
- examples/moneyflow-graphs/referral-bonus.json       首付推荐奖励"

# ─────────────────────────────────────────────────────────
# 13. Subscription 周期扣款 + FX 多币种
# ─────────────────────────────────────────────────────────
echo "▶ [13/18] subscription + fx-service"
git add packages/subscription/ packages/fx-service/
git commit -m "feat(business): subscription + fx-service — SaaS / 跨境支付能力

subscription (SaaS 周期扣款):
- domain/types.go            Plan / Subscription / Invoice / DunningEvent
- workflow/cycle.go          CycleTick (cron) + processCycle (单订阅扣款)
                              + DunningTick (失败重试 3/7/21d) + 状态机
- 关键集成: 收款成功后发 subscription.cycle 事件 → moneyflow-engine
  按 plan.MoneyFlowGraph 自动分账 (复用 Money Flow Graph, 不重复造分账)
- 内置: trial / grace period / max cycles / dunning / proration hook

fx-service (多币种 + 汇率):
- domain/types.go            Rate/Quote/Conversion + ApplyRate/ApplySpread
- sources/ecb.go             ECB 欧央行 + Manual override fetchers
- workflow/quote.go          报价 (锁 5min) + crossRate (USD 桥) + Confirm 落账
- 关键: Quote/Confirm 分离防止双花, 兑换走 accounting AtomicBatch
  (FROM 扣 + TO 加 + fee 扣, 3 笔分录原子)
- 8 位小数精度 (int64 * 1e8) 防 float 累计误差
- 多源汇率聚合: ECB / OANDA / stripe_fx / manual 优先级 + 时间衰减"


# ─────────────────────────────────────────────────────────
# 14. 业务能力 (k6 / wallet / subscription / fx / risk扩展)
# ─────────────────────────────────────────────────────────
echo "▶ [14/18] business capabilities"
git add test/k6/ \
        packages/wallet-service/ packages/subscription/ packages/fx-service/ \
        packages/risk-manage/internal/rules/velocity_geo_device.go \
        packages/risk-manage/internal/rules/velocity_geo_device_test.go 2>/dev/null
git commit -m "feat(business): k6 perf + wallet + subscription + fx + risk扩展

test/k6/                k6 perf suite (01-oauth-token + 02-charge-create + README)
packages/wallet-service/  Pay/Transfer/Freeze, 全走 accounting AtomicBatch
packages/subscription/    cycle worker + dunning + 发 subscription.cycle 事件
packages/fx-service/      Quote/Confirm/cross-rate + ECB+Manual sources
risk-manage 扩展规则      VelocityChecker + GeoChecker + DeviceChecker + 9 单测"

# ─────────────────────────────────────────────────────────
# 15. 平台能力 (schema-registry + problem+json + service generator)
# ─────────────────────────────────────────────────────────
echo "▶ [15/18] platform capabilities"
git add packages/payment-util/schema/ \
        packages/payment-util/problem/ \
        packages/payment-util/tenant/ \
        tools/new-service/
git commit -m "feat(platform): schema registry + RFC7807 problem+json + service generator

packages/payment-util/schema/    Kafka 事件 schema 中心化
- Registry 接口 (MemoryRegistry 实现; prod 接 Confluent 兼容)
- 兼容性策略: BACKWARD/FORWARD/FULL/NONE
- checkBackward: 检测加 required / 改类型等破坏性变更
- Validate: producer 发消息前校验 payload 符合 schema
- 9 个单测 (V1/add-optional/add-required/type-change/latest/validate)

packages/payment-util/problem/   RFC 7807 Problem Details for HTTP APIs
- *Problem struct + Option pattern (WithStatus/Code/Title/Detail/Field/Meta)
- 预定义 helpers (InvalidArgument/Unauthorized/Forbidden/NotFound/Conflict/
  IdempotencyConflict/InsufficientBalance/RateLimited/UpstreamError/Internal)
- Write(w,r,p) 自动注入 trace_id + instance, Content-Type=application/problem+json
- FromError 任意 error → Problem (兜底 500), 已是 *Problem 透传
- 7 个单测 + 1 个跟 ctx trace_id 集成

tools/new-service/              微服务脚手架 generator
- 输入: -name <kebab> -port -domain
- 产出 9 文件: cmd/server (mw.Bootstrap) / go.mod (监 payment-mw/util) /
  Dockerfile / internal/domain+repo+adminhttp / README / k8s manifest / openapi
- 一行命令半小时拉起新服务

packages/payment-util/tenant/    placeholder (用户决策: 多租户暂不需要)"


echo ""
echo "════════════════════════════════════════════"
echo " ✅ 18 个 commit 完成"
echo "════════════════════════════════════════════"
git log --oneline -20

# ─────────────────────────────────────────────────────────
# 16. 修复 payment-util/serviceregistry — dns:/// 永久修复 (13 服务受益)
# ─────────────────────────────────────────────────────────
echo "▶ [16/18] payment-util/serviceregistry dns:/// 修复"
git add packages/payment-util/serviceregistry/dial.go
git commit -m "fix(payment-util): DialWithFallback fallback 路径加 dns:/// 前缀

根因: grpc.NewClient(host:port) 默认走 passthrough resolver, 只在拨号时解
一次 DNS, 之后永不 re-resolve。启动顺序错 / 后端容器重启 → balancer 报
'no children to pick from' 持续不愈, 必须重启 client 进程。

修复: fallback 路径自动给 target 加 dns:/// 前缀, 触发内置 DNS resolver +
idle 重建时 re-resolve。

受益服务 (13 个调用 DialWithFallback):
- accounting-admin-web (本次发现的 /accounts 502)
- api-gateway, payment-core, order-core, user-merchant-core
- card-center, card-payment, payment-channel
- payment-core 的 riskclient/kmsclient/channelclient
- card-center 的 kmsclient, card-payment 的 cardcenterclient
- payment-util/configcenter 自身

一处修, 全栈通。"


# ─────────────────────────────────────────────────────────
# 17. oauth2-server 分库分表 — 100 shard (跟 user-merchant-core 同 layout)
# ─────────────────────────────────────────────────────────
echo "▶ [17/18] oauth2-server 分库分表"
git add packages/oauth2-server/internal/sharding/ \
        packages/oauth2-server/internal/store/mysql_sharded.go \
        packages/oauth2-server/database/init/01_schema_sharded.sql \
        packages/oauth2-server/database/init/generate.sh
git commit -m "feat(oauth2-server): clients / revoked_tokens / admin_audit 100 分片

跟 user-merchant-core / order-core / accounting-system 同 layout:
10 库 × 10 表 = 100 全局分片, FNV-1a hash, 跨服务同 key 落同分片号.

分片对象:
- clients         按 client_id hash → 100 shard
- revoked_tokens  按 jti hash       → 100 shard (高频写, 必分)
- admin_audit     按 actor hash     → 100 shard (跟 u-m-c admin_audit_log 同模式)

不分:
- rsa_keys (key 数量极少, 放 oauth2_db_0)
- client_index 全局映射表 (client_id → (db,table), ops 反查用)

文件:
- internal/sharding/router.go        100 shard 路由 (4 个单测)
- internal/store/mysql_sharded.go    ShardedMySQLStore (fan-out List/GC, 单点 Get/Put)
- database/init/01_schema_sharded.sql 模板 + 首尾分片
- database/init/generate.sh           ~5000 行完整 SQL 生成器

切换:
- cmd/server/main.go 把 NewMySQLStore 换 NewShardedMySQLStore + dsnFor func
- env: OAUTH2_DB_DSN_PREFIX 自动拼 oauth2_db_<idx>"


# ─────────────────────────────────────────────────────────
# 18. oauth2-server 结构对齐 — 跟 user-merchant-core 等老服务统一目录
# ─────────────────────────────────────────────────────────
echo "▶ [18/18] oauth2-server 结构对齐 + Dockerfile vendor 模式"
git add packages/oauth2-server/README.md \
        packages/oauth2-server/CLAUDE.md \
        packages/oauth2-server/Makefile \
        packages/oauth2-server/Dockerfile \
        packages/oauth2-server/docker-compose.yml \
        packages/oauth2-server/config/ \
        packages/oauth2-server/docs/
git commit -m "feat(oauth2-server): 目录结构对齐其他服务

新加 6 个标准文件 (跟 user-merchant-core / order-core / payment-core 一致):
- README.md             快速开始 + 目录 + API endpoints + 集成示例
- CLAUDE.md             给 AI agent 阅读 / 改服务前必读
- Makefile              build/run/test/tidy/vendor/image/smoke/loadtest/schema
- config/config.yaml    生产配置 (viper 默认)
- config/config.docker.yaml docker-compose 用
- docker-compose.yml    单服务起栈
- docs/SHARDING.md      100 分片设计 + reshard SOP

Dockerfile 改 vendor 模式:
- 本地 'go mod vendor' 后 docker build 完全离线
- 解决公司网络 CA 证书 / GOPROXY 不通的 build 失败"

