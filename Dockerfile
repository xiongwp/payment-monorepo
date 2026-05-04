# ─── Stage 1: Build React ─────────────────────────────────────────────────────
FROM node:20-alpine AS react-builder

WORKDIR /app

COPY package.json package-lock.json* ./
RUN npm ci

COPY . .
RUN npm run build

# ─── Stage 2: Build Go HTTP backend ───────────────────────────────────────────
#
# 不再用 `-mod=vendor`：vendor/modules.txt 与 go.mod 容易漂移（比如有新依赖
# 进来但忘了重跑 go mod vendor）。改为 `go mod download` 从公网 proxy 拉包，
# 构建时联网即可。
#
# accounting-grpc-api 通过 git clone 放到同级路径，让 go.mod 里
# `replace ../../accounting-grpc-api` 能解析到。私仓需要 GITHUB_TOKEN secret。
FROM golang:1.25-alpine AS go-builder

RUN apk add --no-cache tzdata git ca-certificates

WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG ACCOUNTING_GRPC_API_REF=claude/fix-payment-util-pvFNu
# payment-util 提供 serviceregistry（etcd resolver / DialWithFallback）；
# 跟 accounting-grpc-api 同样需要 sibling clone 让 go.mod 的
# `replace ../../payment-util` 解析到 /src/payment-util。
ARG PAYMENT_UTIL_REF=claude/fix-payment-util-pvFNu
# CACHEBUST 让 sibling clone 步骤可被显式 invalidate。
# 用 --build-arg CACHEBUST=$(date +%s) 强制重 clone。
ARG CACHEBUST=0
RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    echo "cachebust=$CACHEBUST"; \
    TOKEN=""; \
    [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
    if [ -z "$TOKEN" ]; then \
        echo "ERROR: GITHUB_TOKEN is not set; required to clone private xiongwp/* siblings." >&2; \
        echo "       Generate a token at https://github.com/settings/tokens (scope: repo)," >&2; \
        echo "       then run:  GITHUB_TOKEN=ghp_xxx docker compose build" >&2; \
        exit 1; \
    fi; \
    auth="x-access-token:${TOKEN}@"; \
    clone_with_fallback() { \
        repo=$1; ref=$2; \
        url="https://${auth}github.com/xiongwp/${repo}.git"; \
        dest="/src/${repo}"; \
        if git clone --depth=1 --branch "$ref" "$url" "$dest" 2>/dev/null; then \
            return 0; \
        fi; \
        echo "branch '$ref' not found in $repo; falling back to default branch" >&2; \
        git clone --depth=1 "$url" "$dest"; \
    }; \
    clone_with_fallback accounting-grpc-api "$ACCOUNTING_GRPC_API_REF"; \
    clone_with_fallback payment-util "$PAYMENT_UTIL_REF"

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
