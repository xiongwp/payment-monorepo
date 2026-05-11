# oauth2-server — multi-stage build (vendor 模式, 离线 build)
#
# 前置: 本地跑过 go mod tidy && go mod vendor 把依赖打进 vendor/
# 优点: build 完全离线, 不依赖容器内网络/CA

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .

# -mod=vendor: 强制用 vendor/ 里的代码, 不联网拉
RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" \
    -o /out/oauth2-server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/oauth2-server /app/oauth2-server
USER app
EXPOSE 8087
ENTRYPOINT ["/app/oauth2-server"]
