# ─── Stage 1: Build ───────────────────────────────────────────────────────────
#
# 自包含 build：Dockerfile 自己 git-clone 两个同级仓（accounting-grpc-api 和
# payment-util），避免依赖 outer docker-compose 的 context 必须是父目录。
#
# 这样：
#   - docker compose 用 `context: ./accounting-system` 也能构建
#   - docker compose 用 `context: .`（父目录）也能构建
#
# 私仓 clone 需要 token：outer compose 通过 BuildKit secret 传入：
#   services.accounting-system.build.secrets:
#     - GITHUB_TOKEN
# 然后 `GITHUB_TOKEN=ghp_xxx docker compose build`；
# 公仓（或本机 git 已登录 GitHub）不带 secret 也行。
#
# go.mod 里 `replace ../payment-util` 仍然生效 —— git-clone 把两仓放到
# /src/payment-util 和 /src/accounting-grpc-api，和 /src/accounting-system
# 是兄弟关系，相对路径吻合。
#
# vendor/ 和 -mod=vendor 以前生效过，但现在 go.mod 里加了 payment-util，
# vendor/modules.txt 不同步 —— 改走 `go mod download` + 默认 -mod=readonly。

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache make tzdata git ca-certificates

WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
ARG ACCOUNTING_GRPC_API_REF=main
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
    clone_with_fallback payment-util         "$PAYMENT_UTIL_REF"; \
    clone_with_fallback accounting-grpc-api  "$ACCOUNTING_GRPC_API_REF"

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
