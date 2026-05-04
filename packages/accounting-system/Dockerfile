# ─── Stage 1: Build ───────────────────────────────────────────────────────────
#
# 优先使用本地兄弟仓（../payment-util 和 ../accounting-grpc-api），找不到才 git clone。
#
# 默认（推荐，本地开发）：docker-compose.yml 用 BuildKit 的 additional_contexts
# 注入本地兄弟目录，覆盖下面的 placeholder stage：
#
#   services.<svc>.build:
#     additional_contexts:
#       payment-util:         ../payment-util
#       accounting-grpc-api:  ../accounting-grpc-api
#
# 没有设置 additional_contexts（CI / 干净 checkout）时，placeholder 是空的，
# builder 自动 git clone fallback（需要 BuildKit secret GITHUB_TOKEN）。
#
# go.mod 里 `replace ../payment-util` 仍然生效 —— 不论本地还是 clone，
# 两仓都放到 /src/payment-util 和 /src/accounting-grpc-api，和
# /src/accounting-system 兄弟关系，相对路径吻合。

# ─── Sibling source providers (overridden by additional_contexts) ─────────────
# 这些 placeholder stage 的名字必须和 additional_contexts 的 key 一致。
# 当 docker compose 设置 additional_contexts:payment-util=../payment-util 时，
# 本 stage 被替换为本地目录的 build context，COPY --from=payment-util 拿到真实代码。
# 没设置时，placeholder 没有 go.mod，触发 git clone fallback。
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

# Stage sibling sources：override 时拿到真实仓，否则拿到 placeholder（没 go.mod）。
COPY --from=payment-util         / /tmp/payment-util-staged/
COPY --from=accounting-grpc-api  / /tmp/accounting-grpc-api-staged/

# Promote：发现 go.mod 用本地，否则 git clone。
RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    promote() { \
      name=$1; ref=$2; \
      staged="/tmp/${name}-staged"; final="/src/${name}"; \
      if [ -f "${staged}/go.mod" ]; then \
        echo "→ Using local ${name} (additional_context override)"; \
        mkdir -p "${final}"; \
        cp -a "${staged}/." "${final}/"; \
      else \
        echo "→ Local ${name} not provided, git clone fallback (ref=${ref})"; \
        TOKEN=""; \
        [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
        if [ -z "$TOKEN" ]; then \
          echo "" >&2; \
          echo "ERROR: cannot fetch sibling repo '${name}'." >&2; \
          echo "Either:" >&2; \
          echo "  (1) Use docker compose build from monorepo (additional_contexts auto-set); OR" >&2; \
          echo "  (2) Provide GITHUB_TOKEN: GITHUB_TOKEN=ghp_xxx docker compose build" >&2; \
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

# 这个仓自己的代码（outer compose 的 context 就是这个仓的根目录）
COPY . /src/accounting-system

WORKDIR /src/accounting-system
RUN go mod tidy
RUN go mod download

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

# batchtask cron 支持（service 不用，但占位无害）
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
