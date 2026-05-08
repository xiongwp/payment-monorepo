# ─── Stage 1: Build ───────────────────────────────────────────────────────────
#
# Context = accounting-system/ 自身。兄弟仓通过 BuildKit additional_contexts
# 注入到下面 placeholder stage 上：
#
#   services.accounting-service.build:
#     additional_contexts:
#       payment-util:        ../payment-util
#       accounting-grpc-api: ../accounting-grpc-api

FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM alpine:3.19 AS accounting-grpc-api
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache make tzdata git ca-certificates

WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
ARG ACCOUNTING_GRPC_API_REF=main

COPY --from=payment-util         / /tmp/payment-util-staged/
COPY --from=accounting-grpc-api  / /tmp/accounting-grpc-api-staged/

RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    promote() { \
      name=$1; ref=$2; \
      staged="/tmp/${name}-staged"; final="/src/${name}"; \
      if [ -f "${staged}/go.mod" ]; then \
        echo "→ Using local ${name}"; \
        mkdir -p "${final}"; cp -a "${staged}/." "${final}/"; \
      else \
        TOKEN=""; \
        [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
        [ -z "$TOKEN" ] && { echo "ERROR: provide ../${name} or GITHUB_TOKEN" >&2; exit 1; }; \
        url="https://x-access-token:${TOKEN}@github.com/xiongwp/${name}.git"; \
        rm -rf "${final}"; \
        git clone --depth=1 --branch "$ref" "$url" "$final" 2>/dev/null \
          || git clone --depth=1 "$url" "$final"; \
      fi; \
      rm -rf "${staged}"; \
    }; \
    promote payment-util         "$PAYMENT_UTIL_REF"; \
    promote accounting-grpc-api  "$ACCOUNTING_GRPC_API_REF"

# accounting-system 自己的代码（context = .）
COPY . /src/accounting-system

WORKDIR /src/accounting-system
# GOPROXY 多源 + 重试：抗 sumdb / 单一 proxy 网络抖动 (storage.googleapis.com EOF 等)
ENV GOPROXY=https://proxy.golang.org,https://goproxy.cn,direct
ENV GOSUMDB=off
ENV GOINSECURE=goproxy.cn
RUN for i in 1 2 3 4 5; do go mod tidy && go mod download && exit 0; echo "[go mod] attempt $i failed; retry after 5s..." >&2; sleep 5; done; echo "[go mod] all 5 attempts failed; aborting build" >&2; exit 1

# 两个 binary 一次构建 —— service 和 batchtask 共享同一 image，
# 通过 docker-compose 的 command/entrypoint 字段选择跑哪个。
# 这避免了 service 和 batchtask 单独 build 时 image 版本不一致导致的
# 资金安全 bug（典型场景：service 已部署 cut_date 修复，batchtask 仍用旧
# 代码 → recovery 写入的 cut_date 与原 booking 不一致 → 试算不平）。
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/accounting-system ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/accounting-batchtask ./cmd/batchtask

# ─── Stage 2: Runtime ─────────────────────────────────────────────────────────
# 使用极简 alpine 镜像（~8MB），不含 Go 工具链（运行期无需）。
# 同 image 同时支持：
#   service mode    → CMD /app/accounting-system        (gRPC + workers, USER appuser)
#   batchtask mode  → /app/entrypoint.sh                (busybox crond, 需要 user: root)
FROM alpine:3.19 AS runtime

RUN apk --no-cache add ca-certificates tzdata netcat-openbsd busybox-suid && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone && \
    addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

# 两个 binary
COPY --from=builder /out/accounting-system    /app/accounting-system
COPY --from=builder /out/accounting-batchtask /app/accounting-batchtask
COPY --from=builder /src/accounting-system/config /app/config

# batchtask cron 支持（service 不用，但占位无害）。
COPY deploy/cron/crontab           /etc/crontabs/root
COPY deploy/cron/run-batchtask.sh  /app/run-batchtask.sh
COPY deploy/cron/entrypoint.sh     /app/entrypoint.sh
RUN chmod +x /app/accounting-system \
             /app/accounting-batchtask \
             /app/run-batchtask.sh \
             /app/entrypoint.sh

RUN mkdir -p /app/logs && \
    chown -R appuser:appgroup /app

EXPOSE 50051

HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
    CMD nc -z localhost 50051 || exit 1

# 默认 service mode；batchtask compose 服务需 override CMD + user: root
USER appuser
CMD ["/app/accounting-system"]
