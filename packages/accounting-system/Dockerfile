# ─── Stage 1: Build ───────────────────────────────────────────────────────────
#
# Sibling repos (payment-util / accounting-grpc-api / order-core) **必须** 通过
# docker compose 的 additional_contexts 注入。Dockerfile 不再保留 placeholder
# fallback —— 之前留 'FROM alpine AS xxx' 占位 stage 反而让 BuildKit 优先用
# 缓存的 placeholder 结果（CACHED [order-core 2/2]）而忽略 additional_context
# 的真实路径，导致 'Local order-core not provided' 死循环。
#
# 现在的契约：build 时 additional_contexts 三个 key 必填；缺了直接 hard-fail。
#
#   docker-compose.yml:
#     services.accounting-service.build:
#       additional_contexts:
#         payment-util:        ../payment-util
#         accounting-grpc-api: ../accounting-grpc-api
#         order-core:          ../order-core
#
# CI / 没有本地 sibling 的环境：先 git clone 三个 sibling 到 packages/ 再
# docker compose build；不再支持 GITHUB_TOKEN 跑时拉取（对 monorepo 工作流
# 几乎用不到，且增加 build 时间和失败面）。
#
# go.mod 里的 replace ../<sibling> 在容器内保持有效（COPY 后落到 /src/<sibling>，
# 与 /src/accounting-system 同级，相对路径不变）。

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache make tzdata git ca-certificates

WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

# 三个 sibling 各自 COPY 到 /src/<name>。
#
# 关键：用 '/.' 而不是 '.'。BuildKit 对 filesystem context 的路径解析有 quirk：
#   COPY --from=name . /dest/   有时只拷部分（实测漏 go.mod）
#   COPY --from=name /. /dest/  始终拷整个 context root（含所有 hidden / dot files）
# `/.` 是最稳的写法，类似 Bash 里 `cp -a src/. dst/` 的语义。
#
# 不再 stage placeholder ——一旦 additional_contexts 没配，build 直接报
# "failed to compute cache key: not found"，比静默走 placeholder 误导小。
COPY --from=payment-util         /. /src/payment-util/
COPY --from=accounting-grpc-api  /. /src/accounting-grpc-api/
COPY --from=order-core           /. /src/order-core/

# 这个仓自己的代码（outer compose 的 context 就是这个仓的根目录）
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
