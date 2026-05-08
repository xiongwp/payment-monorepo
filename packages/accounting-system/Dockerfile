# ─── Stage 1: Build ───────────────────────────────────────────────────────────
#
# **Build context 必须是 packages/ 父目录**（不是 accounting-system/ 自身）。
# docker-compose.yml 里设置：
#
#   services.accounting-service.build:
#     context:    ..                       # = packages/
#     dockerfile: accounting-system/Dockerfile
#
# 这样所有 sibling 仓 (payment-util / accounting-grpc-api / order-core /
# accounting-system) 都在同一个 build context 下，普通 COPY 即可拷进来。
# 不再依赖 BuildKit additional_contexts —— 那东西在 Docker Desktop 4.70 /
# BuildKit 当前版本上对 filesystem context 解析有 bug：
#   - COPY --from=ctx . /dst/     只拷部分（漏 go.mod）
#   - COPY --from=ctx /. /dst/    同样漏
#   - 加了 'FROM alpine AS ctx' placeholder 又会优先走 placeholder 缓存
# 排查 1 整天后弃用，改最朴素的 monorepo 模式（all-in-one context）。

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache make tzdata git ca-certificates

WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

# build context 现在是 packages/。所有 sibling 都直接 COPY 进来：
COPY payment-util         /src/payment-util/
COPY accounting-grpc-api  /src/accounting-grpc-api/
COPY order-core           /src/order-core/
COPY accounting-system    /src/accounting-system/

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
# build context 是 packages/ 父目录，所以路径是 accounting-system/deploy/cron/...
COPY accounting-system/deploy/cron/crontab           /etc/crontabs/root
COPY accounting-system/deploy/cron/run-batchtask.sh  /app/run-batchtask.sh
COPY accounting-system/deploy/cron/entrypoint.sh     /app/entrypoint.sh
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
