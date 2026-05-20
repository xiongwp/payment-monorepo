FROM golang:1.22-alpine AS builder
RUN apk add --no-cache git ca-certificates
ENV GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org
WORKDIR /src
COPY . .
# go.sum 历史漂移, tidy 同步 fx / dig 等新依赖.
RUN go mod tidy && go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/refund-engine ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /out/refund-engine /app/refund-engine
EXPOSE 8080
ENTRYPOINT ["/app/refund-engine"]
