# oauth2-server — multi-stage build
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/oauth2-server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/oauth2-server /app/oauth2-server
USER app
EXPOSE 8087
ENTRYPOINT ["/app/oauth2-server"]
