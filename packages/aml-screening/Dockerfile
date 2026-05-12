# aml-screening — multi-stage build (跟 oauth2-server 同模式, go mod download 不走 vendor)
FROM golang:1.22-alpine AS build

# vendor 模式: 容器 build 不需要网络. 前提: 本地已跑 `GOWORK=off go mod vendor`.
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=vendor

WORKDIR /src
# 把整个源码 (含 vendor/) 复制进来
COPY . .

# -mod=vendor 强制用 vendor/, 不联网.
RUN go build -trimpath -ldflags="-s -w" -o /out/aml-screening ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/aml-screening /app/aml-screening
USER app
EXPOSE 8088
ENTRYPOINT ["/app/aml-screening"]
