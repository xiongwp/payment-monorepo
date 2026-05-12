FROM golang:1.22-alpine AS build

# vendor 模式: 容器零网络. 前提: 本地已跑 `GOWORK=off go mod vendor`.
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=vendor

WORKDIR /src
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/data-rights ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/data-rights /app/data-rights
USER app
EXPOSE 8091
ENTRYPOINT ["/app/data-rights"]
