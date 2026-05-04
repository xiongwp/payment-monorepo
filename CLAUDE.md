# accounting-admin-web

accounting-system 的管理前端（React + TypeScript + Vite）。运维查账户余额 / 系统配置 / hot-account 配置 / 日切状态。

## 定位

```
浏览器  ─HTTP──→  accounting-admin-web (nginx + React SPA)  ─HTTP──→  accounting-system (:8888)
```

前端调用 accounting-system 的 `/admin/*` HTTP 端点，不直接碰 DB / gRPC。

## 页面结构

| Path | 作用 |
|---|---|
| `/platform-accounts` | 按 business_type 列出 100 个 fleet 平台账户 + 余额 |
| `/platform-accounts/snapshots` | 指定 cut_date 的账户快照 |
| `/business-types` | 列出 / 新增 business_type（account_business_type_info）|
| `/hot-accounts` | 热点账户白名单 CRUD |
| `/buffer-accounts` | 缓冲记账账户配置 |
| `/system-config` | 通用 kv 配置 CRUD |
| `/tcc` | TCC 状态查询 / 恢复 |

## 本地运行

```bash
npm install
npm run dev    # Vite dev server，proxy /admin/* → http://localhost:8888
```

## 环境变量

- `VITE_ADMIN_HTTP_ADDR`：accounting-system admin HTTP 基址；默认 `http://localhost:8888`
- `VITE_ADMIN_TOKEN`：X-Admin-Token 头值；生产必填

## 依赖约束

- 金额显示统一**币种精度** × 10⁴（accounting 内部 storage 值 / 10⁴ = major units，再按币种 precision 格式化）
- 所有"写"操作 POST / PUT / DELETE 后必须触发对应的 `/admin/reload/*` 扇出，让多实例 cache 同步
- 避免在前端做业务判断（enabled / disabled 等规则后端是真源）
