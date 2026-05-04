# api-gateway

公网 HTTP 入口。鉴权 / 限流 / 路由到内部 gRPC 服务。

## 端口

| 端口 | 用途 | 暴露 |
|---|---|---|
| 8080 | 公网 HTTP（`/v1/*` + `/health`） | Internet |
| 8081 | Admin HTTP（`/admin/health` + 路由热重载） | 内网 |
| 9090 | Prometheus `/metrics` | 内网 |

## 中间件链

公网请求按 **外 → 内** 顺序穿过：

```
recovery → logging → rate-limit → auth → handler
```

- **recovery**：handler panic 兜底，转 500，避免单条坏请求拖死进程。
- **logging**：只记 method/path/status/duration/size，不记 body（防 PII 泄露）。
- **rate-limit**：per-IP（防匿名扫描） + per-merchant（X-Merchant-ID 头）双维度。
- **auth**：API key 校验，`X-API-Key` 头，`subtle.ConstantTimeCompare` 防 timing attack。
- `/health` 路径豁免限流 + 鉴权（K8s 探针）。

Admin HTTP 单独包一层 `X-Admin-Token`，token 同样 constant-time 比较；空 token 启动报 ERROR。

## 配置

参见 `config/config.yaml`。生产部署：

- ConfigMap 挂载 `/etc/api-gateway/config.yaml`
- Secret → 环境变量注入：`ADMIN_HTTP_TOKEN`、`auth.tokens` 里的 KMS 密文（`kms:v1:...` 格式 → 启动时透明解密，待 KMS 客户端接入）

## 构建

```bash
make build
make test                  # go test -race ./...
GITHUB_TOKEN=ghp_xxx docker build --secret id=GITHUB_TOKEN,env=GITHUB_TOKEN .
```

## 当前状态

骨架版本：仅有 `/health` + `/v1/ping`。下游 gRPC stub 接入（order-core / payment-core / user-merchant-core）由后续 PR 完成。
