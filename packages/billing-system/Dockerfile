FROM golang:1.22-alpine AS builder
RUN apk add --no-cache git ca-certificates
ENV GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org
WORKDIR /src
COPY . .
# go.sum 历史漂移 (fx 等新增依赖未补), tidy 自动同步.
RUN go mod tidy && go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/billing-system ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /out/billing-system /app/billing-system
COPY configs /app/configs
EXPOSE 8080
ENTRYPOINT ["/app/billing-system"]
