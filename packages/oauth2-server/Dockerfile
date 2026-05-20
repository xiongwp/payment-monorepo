# oauth2-server — multi-stage build
#
# 之前用 vendor 模式跟 monorepo 根 go.work (1.26.1) 撞导致 modules.txt 缺 explicit
# 标记, 改回标准 go mod download. GOPROXY 走 proxy.golang.org direct fallback,
# 不依赖国内代理 (goproxy.cn TLS 证书在 alpine 上偶尔有问题).

FROM golang:1.22-alpine AS build

# Go 代理: 优先官方, 失败回 direct (从源仓库克隆)
ENV GOPROXY=https://proxy.golang.org,direct \
    GOSUMDB=sum.golang.org \
    CGO_ENABLED=0 \
    GOOS=linux

# alpine 自带的 ca-certificates 配合 GOPROXY https
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# 1) COPY 全量源码 + tidy + build
# go.sum 历史漂移 (fx / dig 等新依赖未补), 用 go mod tidy 让 build 自己补齐再下载.
COPY . .
RUN go mod tidy && go mod download
RUN go build -trimpath -ldflags="-s -w" -o /out/oauth2-server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/oauth2-server /app/oauth2-server
USER app
EXPOSE 8087
ENTRYPOINT ["/app/oauth2-server"]
