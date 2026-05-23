# split-payment — SP-AC-7 PROD1: 真实生产 Dockerfile.
#
# 设计:
#   - multi-stage: golang:1.25 builder → gcr.io/distroless/static-debian12 runtime
#   - 不带 shell 不带 包管理器, 攻击面最小
#   - 启动期需要 ../payment-util / ../accounting-system replace, 用 additional_contexts (compose buildx) 或 monorepo-root 构建上下文 (CI)
#
# 用法 (本地):
#   cd /Users/bytedance/source/payment-monorepo
#   docker build -f packages/split-payment/Dockerfile -t split-payment:dev .
#
# 用法 (CI):
#   build context = monorepo 根; Dockerfile 路径要从根算起 (packages/...).
#
# K8s 上跑时, 容器跑 USER 65532 (nonroot), 监听 :9098 (gRPC) + :9099 (admin).
# 资源建议: 200m CPU / 256Mi mem (起步), HPA 据 cpu_utilization 扩.

# ─── Stage 1: builder ────────────────────────────────────────────────────────
FROM golang:1.25-bookworm AS builder

# Go module cache 复用, CI 时 layer 命中.
ENV GOPROXY=https://proxy.golang.org,direct
ENV CGO_ENABLED=0
ENV GOOS=linux

# monorepo 根 → /src
WORKDIR /src

# 复制依赖清单优先 (利用 layer cache)
COPY packages/split-payment/go.mod packages/split-payment/go.sum packages/split-payment/
COPY packages/payment-util/ packages/payment-util/
COPY packages/accounting-system/go.mod packages/accounting-system/go.sum packages/accounting-system/
COPY packages/accounting-system/internal/ packages/accounting-system/internal/
# split-payment 通过 replace 引用 accounting-system 的 kitex_gen（gRPC client stubs）；
# 漏拷会让 internal/clients/accounting_grpc.go 找不到包 → go build 失败。
COPY packages/accounting-system/kitex_gen/ packages/accounting-system/kitex_gen/

# tidy 拉依赖
WORKDIR /src/packages/split-payment
RUN go mod download

# 复制业务代码 (放最后, 改业务时不重 download)
COPY packages/split-payment/ /src/packages/split-payment/

# 编译 — static binary, strip debug symbols 缩 image 体积
RUN go build \
    -ldflags="-s -w -X main.buildVersion=$(date +%Y%m%d-%H%M%S)" \
    -o /out/split-payment \
    ./cmd/server

# ─── Stage 2: runtime ────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

# 标签 (供 docker inspect / K8s 调度看)
LABEL org.opencontainers.image.title="split-payment"
LABEL org.opencontainers.image.description="Money Flow Orchestration service (SP-AC-7)"
LABEL org.opencontainers.image.source="https://github.com/xiongwp/payment-monorepo"

COPY --from=builder /out/split-payment /usr/local/bin/split-payment

# distroless nonroot user (65532:65532)
USER nonroot:nonroot

EXPOSE 9098 9099

# 默认 entrypoint, 让容器跑 split-payment server.
ENTRYPOINT ["/usr/local/bin/split-payment"]
