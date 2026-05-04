FROM golang:1.26.1 AS builder

WORKDIR /app

# 利用缓存
COPY go.mod go.sum ./
RUN go mod tidy

COPY . .

RUN go build -o id-server ./cmd/server

# 运行镜像（更小）
FROM debian:bookworm-slim

WORKDIR /app
COPY --from=builder /app/id-server .

CMD ["./id-server"]