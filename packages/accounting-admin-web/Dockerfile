# ─── Stage 1: Build React ─────────────────────────────────────────────────────
FROM node:20-alpine AS react-builder

WORKDIR /app

COPY package.json package-lock.json* ./
RUN npm ci

COPY . .
RUN npm run build

# ─── Stage 2: Build Go HTTP backend ───────────────────────────────────────────
#
# 优先用本地兄弟仓 ../accounting-grpc-api 和 ../payment-util，找不到才 git clone。
# docker-compose.yml 用 BuildKit additional_contexts 注入：
#   build:
#     additional_contexts:
#       payment-util:        ../payment-util
#       accounting-grpc-api: ../accounting-grpc-api

# Sibling source provider placeholders。
FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS accounting-grpc-api
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.25-alpine AS go-builder

RUN apk add --no-cache tzdata git ca-certificates

WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG ACCOUNTING_GRPC_API_REF=main
ARG PAYMENT_UTIL_REF=main

COPY --from=payment-util         / /tmp/payment-util-staged/
COPY --from=accounting-grpc-api  / /tmp/accounting-grpc-api-staged/

RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    promote() { \
      name=$1; ref=$2; \
      staged="/tmp/${name}-staged"; final="/src/${name}"; \
      if [ -f "${staged}/go.mod" ]; then \
        echo "→ Using local ${name} (additional_context override)"; \
        mkdir -p "${final}"; cp -a "${staged}/." "${final}/"; \
      else \
        echo "→ Local ${name} not provided, git clone fallback (ref=${ref})"; \
        TOKEN=""; \
        [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
        if [ -z "$TOKEN" ]; then \
          echo "ERROR: cannot fetch sibling '${name}' — provide ../${name} via additional_contexts OR GITHUB_TOKEN secret." >&2; \
          exit 1; \
        fi; \
        auth="x-access-token:${TOKEN}@"; \
        url="https://${auth}github.com/xiongwp/${name}.git"; \
        rm -rf "${final}"; \
        git clone --depth=1 --branch "$ref" "$url" "$final" 2>/dev/null \
          || git clone --depth=1 "$url" "$final"; \
      fi; \
      rm -rf "${staged}"; \
    }; \
    promote payment-util         "$PAYMENT_UTIL_REF"; \
    promote accounting-grpc-api  "$ACCOUNTING_GRPC_API_REF"

# backend 自己的代码放到 /src/accounting-admin-web/backend，
# 和 go.mod 里 `replace ../../accounting-grpc-api` 的相对路径一致
# （../../ 从 backend/ 走两层到 /src）。
COPY backend /src/accounting-admin-web/backend

WORKDIR /src/accounting-admin-web/backend
RUN go mod tidy
RUN go mod download

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
        -ldflags="-s -w" \
        -o bin/api-server ./cmd/server

# ─── Stage 3: Runtime (nginx + Go backend) ────────────────────────────────────
FROM nginx:1.25-alpine

RUN apk --no-cache add ca-certificates tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

# Go backend binary
COPY --from=go-builder /src/accounting-admin-web/backend/bin/api-server /usr/local/bin/api-server

# React static files
COPY --from=react-builder /app/dist /usr/share/nginx/html

# nginx config (proxies /api/* → Go backend on localhost:9090)
RUN rm -f /etc/nginx/conf.d/default.conf
COPY nginx/default.conf /etc/nginx/conf.d/default.conf

# Startup script: starts Go backend then nginx
COPY nginx/docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

EXPOSE 80

HEALTHCHECK --interval=15s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- http://localhost/health || exit 1

ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["nginx", "-g", "daemon off;"]
