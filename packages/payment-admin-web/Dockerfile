# Pin 到 20.18 — Node 20.20+ 在 ESM/CJS 双包解析上变严，会导致 vite 5.4.x
# 'import { build } from "esbuild"' 报 "Named export 'build' not found"。
# 20.18 是上一个稳定 LTS，与 vite 5.4 + esbuild 0.21 完美兼容。
FROM node:20.18-alpine AS build
WORKDIR /src
COPY package.json package-lock.json* ./
# 用 npm ci 严格按 lock 装：避免 npm install 自动升级 vite/esbuild 到不兼容的新版。
RUN npm ci --no-audit --no-fund
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=build /src/dist /usr/share/nginx/html
COPY nginx/nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
