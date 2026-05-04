# payment-admin-web

支付运营后台前端（React + TypeScript + Vite）+ BFF backend。运营看订单 / 退款 / 对账 / 商户 KYC 审核。

## 定位

```
运营人员  ─HTTPS─→  payment-admin-web (nginx + React SPA)
                         │
                         ↓ HTTP
                      backend (Go, BFF 层)
                         │ gRPC
                   ┌─────┴──────┬─────────────┐
                   ↓            ↓             ↓
              order-core  user-merchant-core  accounting-system
```

## 页面结构

| Path | 作用 |
|---|---|
| `/orders` | 订单列表 / 查询 / 详情（PI + Charges + Refunds）|
| `/orders/:id` | 单笔订单详情 + 渠道流水 |
| `/refunds` | 退款 |
| `/merchants` | 商户管理 / KYC 审核 |
| `/merchants/:id` | 商户详情 + 渠道密钥 |
| `/reconciliation` | 日切对账结果（from reconciliation worker） |
| `/metrics` | 链路指标 embed |

## 后端 BFF

- 端口默认 `:8080`
- 调 order-core / user-merchant-core / accounting-system 的 gRPC
- 做 **聚合 + 权限过滤**（前端一次请求 vs 后端多次 gRPC 聚合）
- 不持久化（除 session）

## 本地运行

```bash
# 纯前端 dev
npm install && npm run dev

# BFF 后端
cd backend && go run .

# 完整 stack（如果 stack/ 目录存在）
./deploy.sh up
```

## 环境变量（前端）

- `VITE_BACKEND_URL`：BFF 地址，默认 `http://localhost:8080`

## 环境变量（后端）

- `ORDER_GRPC_ADDR` / `USER_MERCHANT_GRPC_ADDR` / `ACCOUNTING_GRPC_ADDR`
- `ACCOUNTING_ADMIN_HTTP_ADDR`：accounting-system admin HTTP 基址

## 依赖约束

- 金额展示走**一个**转换函数：`displayAmount(minor_units, currency)` → `"¥100.00"`，不要散落在各组件里
- 列表分页必须走 server-side（pi 表百万级，client-side 分页会 OOM）
- PII 字段（手机 / 邮箱 / 卡号）在 BFF 层按权限脱敏
