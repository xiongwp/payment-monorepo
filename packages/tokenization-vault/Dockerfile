FROM golang:1.22-alpine AS build

ENV GOPROXY=https://goproxy.cn,https://goproxy.io,https://proxy.golang.org,direct \
    GOSUMDB=off \
    CGO_ENABLED=0 \
    GOOS=linux

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum* ./

COPY . .
RUN go mod tidy && \
    go build -trimpath -ldflags="-s -w" -o /out/tokenization-vault ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/tokenization-vault /app/tokenization-vault
USER app
EXPOSE 8089
ENTRYPOINT ["/app/tokenization-vault"]
