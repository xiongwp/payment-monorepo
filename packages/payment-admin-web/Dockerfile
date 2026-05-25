FROM node:20-alpine AS build
WORKDIR /src
COPY package.json package-lock.json* ./
# 用 npm ci 严格按 lock 装：避免 npm install 自动升级 vite/esbuild 到不兼容的新版
# （vite 5.4.x 在 Node 20.20+ 上对 esbuild ESM 解析变严，npm install 拿到新 minor 就崩）。
RUN npm ci --no-audit --no-fund
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=build /src/dist /usr/share/nginx/html
COPY nginx/nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
