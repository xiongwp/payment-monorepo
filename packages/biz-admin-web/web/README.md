# biz-admin-web/web — React SPA (v2)

旧版 (Alpine.js HTML) 在 `../static/`. 这里是 React+Vite 重写, 渐进迁移.

## 开发

```bash
cd packages/biz-admin-web/web
npm install
npm run dev
# Vite dev server 在 http://localhost:5173, proxy /api → 8080
```

## 生产 build

```bash
npm run build
# 输出 ../static-react/
# Go server `cmd/server/main.go` 用 //go:embed static-react/* 把整个 SPA 打进二进制
```

## 迁移路径

| 阶段 | 旧 (Alpine.js) | 新 (React) |
|---|---|---|
| Phase 0 (current) | static/admin.html, p0-services.html, approval.html | web/src — 同等功能 reference, 仅 ApprovalPage 完整实现 |
| Phase 1 | 保留, fall-back | React 作为默认 /; Alpine pages 作为 /v1 |
| Phase 2 | 仅 /v1/* 路径 | React 完整功能, alpine 弃用 |
| Phase 3 | 删 | 移除 alpine; 所有页面 React |

## 技术栈

- **React 18** + Hooks
- **TypeScript** 5 (严格模式)
- **React Router 6** (BrowserRouter)
- **TanStack Query** (server state, cache, retry, SSE invalidate)
- **Tailwind CSS** (跟旧版一致, 不引 component lib)
- **Vite** build / dev server

## 跟旧版互操作

- 后端 Go server (`cmd/server/main.go`) 同时 serve `/admin` (旧), `/approval` (旧), `/p0` (旧) 三页面
- 新加 `//go:embed static-react/*` 路由 `/` 默认 React SPA
- Alpine pages 用作 fallback / 文档

## 单测

```bash
npm test  # Vitest + Testing Library (待加)
```

## 跟 Go server 集成

`cmd/server/main.go` 加:

```go
//go:embed static-react/*
var reactAssets embed.FS

// 路由
mux.Handle("/", http.FileServer(http.FS(reactAssets)))
```

build pipeline (CI):
1. `cd web && npm ci && npm run build` 出 `static-react/`
2. `go build` embed 进 binary
3. docker image 含完整 SPA
