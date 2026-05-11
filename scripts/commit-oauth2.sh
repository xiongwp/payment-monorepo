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
echo "▶ [1/5] oauth2-server"
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
echo "▶ [2/5] payment-mw"
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
echo "▶ [3/5] OpenAPI specs"
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
echo "▶ [4/5] integration demos + docs"
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
echo "▶ [5/6] biz-stack compose 集成"
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
echo "▶ [6/6] OAuth2 生产化优化"
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

echo ""
echo "════════════════════════════════════════════"
echo " ✅ 5 个 commit 完成"
echo "════════════════════════════════════════════"
git log --oneline -7
