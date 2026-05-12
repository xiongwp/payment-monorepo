# aml-screening — multi-stage build (跟 oauth2-server 同模式, go mod download 不走 vendor)
FROM golang:1.22-alpine AS build

ENV GOPROXY=https://proxy.golang.org,direct \
    GOSUMDB=sum.golang.org \
    CGO_ENABLED=0 \
    GOOS=linux

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/aml-screening ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/aml-screening /app/aml-screening
USER app
EXPOSE 8088
ENTRYPOINT ["/app/aml-screening"]
