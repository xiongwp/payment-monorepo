# aml-screening — multi-stage build
FROM golang:1.22-alpine AS build

# vendor 历史漂移 (fx 等新依赖 modules.txt 未标 explicit), 不再走 vendor; 改 go mod tidy.
ENV CGO_ENABLED=0 GOOS=linux \
    GOPROXY=https://proxy.golang.org,direct \
    GOSUMDB=sum.golang.org

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY . .

RUN go mod tidy && go mod download
RUN go build -trimpath -ldflags="-s -w" -o /out/aml-screening ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/aml-screening /app/aml-screening
USER app
EXPOSE 8088
ENTRYPOINT ["/app/aml-screening"]
