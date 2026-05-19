FROM golang:1.22-alpine AS build

# vendor 历史漂移 → 改 go mod tidy 联网 build.
ENV CGO_ENABLED=0 GOOS=linux \
    GOPROXY=https://proxy.golang.org,direct \
    GOSUMDB=sum.golang.org

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY . .
# vendor/ 在源里会让 Go 走 vendor 模式 → 显式删 + -mod=mod 强制走 module cache.
RUN rm -rf vendor
RUN go mod tidy && go mod download
RUN go build -mod=mod -trimpath -ldflags="-s -w" -o /out/tokenization-vault ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/tokenization-vault /app/tokenization-vault
USER app
EXPOSE 8089
ENTRYPOINT ["/app/tokenization-vault"]
