# payment-core Dockerfile — 优先用本地兄弟仓，找不到才 git clone。
#
# 默认（推荐）：docker-compose.yml 用 BuildKit additional_contexts 注入本地：
#   build:
#     additional_contexts:
#       payment-util:    ../payment-util
#       kms-manage:      ../kms-manage
#       payment-channel: ../payment-channel
#       risk-manage:     ../risk-manage
#
# 没设置时（CI / 干净 checkout）fallback 到 git clone（需 GITHUB_TOKEN secret）。
# 覆盖分支：--build-arg PAYMENT_UTIL_REF / KMS_MANAGE_REF / PAYMENT_CHANNEL_REF / RISK_MANAGE_REF

# Sibling source provider placeholders。
FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS kms-manage
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS payment-channel
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS risk-manage
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
ARG KMS_MANAGE_REF=main
ARG PAYMENT_CHANNEL_REF=main
ARG RISK_MANAGE_REF=main

# Stage sibling sources.
COPY --from=payment-util    / /tmp/payment-util-staged/
COPY --from=kms-manage      / /tmp/kms-manage-staged/
COPY --from=payment-channel / /tmp/payment-channel-staged/
COPY --from=risk-manage     / /tmp/risk-manage-staged/

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
    promote payment-util    "$PAYMENT_UTIL_REF"; \
    promote kms-manage      "$KMS_MANAGE_REF"; \
    promote payment-channel "$PAYMENT_CHANNEL_REF"; \
    promote risk-manage     "$RISK_MANAGE_REF"

# 这个仓自己的代码
COPY . /src/payment-core

WORKDIR /src/payment-core
# GOPROXY 多源 + 重试：抗 sumdb / 单一 proxy 网络抖动 (storage.googleapis.com EOF 等)
ENV GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
ENV GOSUMDB=off
RUN for i in 1 2 3 4 5; do       go mod tidy && go mod download && break;       echo "[go mod] attempt $i failed; retry after 5s..." >&2;       sleep 5;     done
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
