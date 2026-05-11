# oauth2-server

OAuth 2.0 client_credentials 授权服务 — 商户 / 服务间 RS256 JWT 颁发。

实现 RFC 6749 §4.4 + 7517 (JWKS) + 7662 (introspection) + 7009 (revocation)。

## 快速开始

```bash
# 1. build
make build

# 2. run (dev, in-memory store + 3 seed clients)
OAUTH2_DEV_SEED=1 OAUTH2_ADMIN_TOKEN=admintok make run

# 3. test
make test

# 4. smoke (e2e curl)
bash test/smoke.sh http://localhost:8087
```

## 目录结构

```
oauth2-server/
├── README.md          (本文件)
├── CLAUDE.md          (项目说明 — 给 AI agent 阅读)
├── Makefile           (build / run / test / image)
├── Dockerfile         (multi-stage, vendor 模式离线编译)
├── docker-compose.yml (单服务起栈)
├── api/openapi.yaml   (OpenAPI 3.0 规约)
├── cmd/server/        (入口)
├── client/            (Go SDK)
├── config/            (config.yaml / config.docker.yaml)
├── database/init/     (schema SQL + generator)
├── deploy/            (k8s manifests + Prometheus alerts)
├── docs/              (设计文档)
├── internal/
│   ├── adminhttp/     (HTTP endpoints)
│   ├── audit/         (admin action audit)
│   ├── domain/        (Client / Claims / Token)
│   ├── jwks/          (RSA key store + JWT sign/verify)
│   ├── metrics/       (Prometheus)
│   ├── ratelimit/     (token bucket)
│   ├── sharding/      (100 shard router)
│   └── store/         (memory / mysql / mysql_sharded)
└── test/              (smoke + loadtest)
```

## 配置

`config/config.yaml` (viper 默认搜索 ./config / . / /etc/oauth2-server):

```yaml
server:
  http_port: 8087
issuer: "https://oauth.payment.example.com"
audience: "payment-api"
token_ttl: 3600s
admin_token: "${OAUTH2_ADMIN_TOKEN}"

rsa:
  key_path: "/var/lib/oauth2/oauth-rsa.pem"
  rotation_days: 90
  retention_days: 30

database:
  dsn_prefix: "${OAUTH2_DB_DSN_PREFIX}"   # tcp(host:3306)/oauth2_db_
  sharded: true                           # 走 ShardedMySQLStore

rate_limit:
  client_rps: 20
  client_burst: 40
  ip_rps: 50
  ip_burst: 100

dev:
  seed_clients: false
```

环境变量覆盖配置 (跟其它服务一致, `OAUTH2_xxx_xxx` 映射 yaml 同层级)。

## API 端点

| 路径 | 用途 |
|---|---|
| `POST /oauth2/token` | client_credentials grant |
| `POST /oauth2/introspect` | RFC 7662 |
| `POST /oauth2/revoke` | RFC 7009 |
| `GET  /.well-known/jwks.json` | 公钥 |
| `GET  /.well-known/openid-configuration` | Discovery |
| `POST /admin/clients` | 创建客户端 |
| `POST /admin/clients/{id}/rotate-secret` | 滚 secret |
| `POST /admin/keys/rotate` | 轮换 RSA key |
| `GET  /healthz` / `GET /metrics` | 探针 / Prometheus |

完整 spec: [`api/openapi.yaml`](api/openapi.yaml)

## 集成

业务服务通过 [`payment-mw`](../payment-mw) 一行接入:

```bash
OAUTH2_JWKS_URL=http://oauth2-server:8087/.well-known/jwks.json
OAUTH2_ISSUER=http://oauth2-server:8087
OAUTH2_AUDIENCE=payment-api
```

然后业务代码:

```go
mux.Handle("POST /api/v1/refunds",
    mw.RequireScope("refund:write")(http.HandlerFunc(createRefund)))
```

详见 [`../../examples/oauth2-integration/`](../../examples/oauth2-integration/) 9 个 demo 文件。

## 分库分表 (100 shard)

参见 [`docs/SHARDING.md`](docs/SHARDING.md) — 跟 user-merchant-core / order-core / accounting-system 同 layout。

部署:

```bash
# 生成 ~5000 行完整 schema
bash database/init/generate.sh > database/init/02_schema_full.sql
mysql -h <host> -uroot -p < database/init/02_schema_full.sql
```
