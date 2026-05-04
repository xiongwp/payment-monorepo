# payment-admin-web

支付平台管理后台：订单查询、渠道路由探测、Webhook 调试、KMS 密钥管理。

跟 accounting-admin-web 架构一致 —— 前端 Vite + React + Antd，后端一个 Go BFF 把
order-core / payment-core / kms-manage 三个 gRPC 服务聚合成 JSON HTTP。

```
┌──────────┐  JSON  ┌──────────────────────┐  gRPC  ┌────────────────┐
│ Vue/React│ ─────► │payment-admin-backend │ ─────► │ order-core     │
│  (nginx) │        │      (BFF)           │        │ payment-core   │
└──────────┘        └──────────────────────┘        │ kms-manage     │
                                                    └────────────────┘
```

## 模块

```
backend/               Go BFF，对前端暴露 /api/*
  cmd/server           入口
  internal/clients     gRPC client 集合
  internal/handler     HTTP handler（orders / channels / kms / dashboard）
src/
  api/                 axios + 请求封装
  layouts/AppLayout    侧边栏 + 头
  pages/
    Dashboard          首屏概况
    Orders             订单列表 + 详情
    Channels           路由探测 + Webhook 调试
    KMS                Master key 列表 + 线上加密/解密辅助
```

## 本地开发

前置：
1. 三个后端服务跑起来（端口按默认）：
   - order-core:         `:9091`
   - payment-core:       `:9090`
   - kms-manage:         `:9290`
2. 本仓的后端 + 前端：

```bash
# 后端（BFF）
cd backend
go run ./cmd/server
# → :9190

# 前端（另一个终端）
npm install
npm run dev
# → :3100，/api → :9190
```

## API 端点

| 方法 | 路径 | 用途 |
|---|---|---|
| GET  | /api/dashboard/summary     | 首屏概况 |
| GET  | /api/orders?mch_id=&page=  | PaymentIntent 列表 |
| GET  | /api/orders/{id}           | PaymentIntent 详情 |
| GET  | /api/orders/{id}/charges   | 该订单所有 Charge |
| GET  | /api/orders/{id}/refunds   | 该订单所有 Refund |
| POST | /api/channels/routes/probe | 路由探测（给定 country/method/amount） |
| POST | /api/channels/webhook-test | Webhook 原文解析 |
| GET  | /api/kms/keys              | Master key 列表 |
| POST | /api/kms/encrypt           | 加密文本 |
| POST | /api/kms/decrypt           | 解密文本 |

所有返回统一 shape：`{code, message, data}`，code=0 为成功。

## 鉴权

BFF 支持一个简单的 `ADMIN_BEARER_TOKEN` 环境变量：非空则所有 `/api/*` 要求
`Authorization: Bearer <token>`。前端把 token 存在 `localStorage.admin_token`。

## Docker 部署

### 单独起 admin（后端要你自己保证 order-core / payment-core / kms-manage 可达）

```bash
cd /home/user/payment-admin-web
docker compose build
docker compose up
# → http://localhost:8080
```

### 一键起全栈（推荐，e2e 测试用）

`deploy.sh` 把 `kms-manage / payment-channel / order-core / payment-core / payment-admin-web`
五个服务拼在 `payment-stack` docker network 上一起跑。详见 [deploy/README.md](deploy/README.md)。

```bash
# 首次：产 kms master key
./deploy.sh init-kms

# 起全栈
./deploy.sh up

# 健康检查
./deploy.sh check

# 停
./deploy.sh down
```

起完后访问 <http://localhost:8080> 即可看到管理后台；链路：

```
浏览器 :8080 (nginx)
   └── /api → payment-admin-backend (BFF, 内网 9190)
        ├── gRPC → order-core:9091
        ├── gRPC → payment-core:9090 ─ gRPC → payment-channel:9092 ─ fake adapter
        └── gRPC → kms-manage:9290
```
