# payment-core Dockerfile — 自包含构建
#
# 以前要求 docker build context 是"父目录"以看到 ../payment-util / ../kms-manage /
# ../payment-channel / ../risk-manage。当 outer docker-compose 用
# `context: ./payment-core` 时，sibling COPY 会失败。
#
# 现在 Dockerfile 自己 git-clone 四个兄弟仓到 /src/<name>，
# go.mod 里的 `replace ../<name>` 相对路径仍然成立。
#
# 私仓 clone 通过 BuildKit secret 传 token：
#   services.payment-core.build.secrets:
#     - GITHUB_TOKEN
# 然后 GITHUB_TOKEN=ghp_xxx docker compose build；
# 公仓不带 secret 也行。
#
# 覆盖分支：--build-arg PAYMENT_UTIL_REF / KMS_MANAGE_REF / PAYMENT_CHANNEL_REF /
# RISK_MANAGE_REF

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
ARG KMS_MANAGE_REF=main
ARG PAYMENT_CHANNEL_REF=main
ARG RISK_MANAGE_REF=main
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
    clone_with_fallback payment-util    "$PAYMENT_UTIL_REF"; \
    clone_with_fallback kms-manage      "$KMS_MANAGE_REF"; \
    clone_with_fallback payment-channel "$PAYMENT_CHANNEL_REF"; \
    clone_with_fallback risk-manage     "$RISK_MANAGE_REF"

# 这个仓自己的代码
COPY . /src/payment-core

WORKDIR /src/payment-core
RUN go mod tidy
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/payment-core ./cmd/server

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/payment-core /app/payment-core
# 构建上下文是 payment-core 自身目录，config 相对路径就是 config/
COPY config /app/config
USER app
EXPOSE 9090 9190
CMD ["/app/payment-core"]
