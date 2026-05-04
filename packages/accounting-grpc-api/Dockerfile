FROM golang:1.21-alpine AS builder

RUN apk add --no-cache git make protobuf-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN make generate && make build

FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

COPY --from=builder /app/bin/grpc-server .
COPY --from=builder /app/config ./config

ENV TZ=Asia/Shanghai

EXPOSE 9090

CMD ["./grpc-server"]
