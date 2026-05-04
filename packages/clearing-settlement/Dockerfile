# clearing-settlement Dockerfile — 自包含构建（与 payment-core / api-gateway 风格一致）。

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG ACCOUNTING_GRPC_API_REF=main
ARG PAYMENT_UTIL_REF=main

# CACHEBUST 让 sibling clone 步骤可被显式 invalidate。
# 用 --build-arg CACHEBUST=$(date +%s) 强制重 clone。
ARG CACHEBUST=0
RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    echo "cachebust=$CACHEBUST"; \
    TOKEN=""; \
    [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
    if [ -z "$TOKEN" ]; then \
        echo "ERROR: GITHUB_TOKEN is not set; required to clone private xiongwp/* siblings." >&2; \
        exit 1; \
    fi; \
    auth="x-access-token:${TOKEN}@"; \
    clone_with_fallback() { \
        repo=$1; ref=$2; \
        url="https://${auth}github.com/xiongwp/${repo}.git"; \
        dest="/src/${repo}"; \
        if git clone --depth=1 --branch "$ref" "$url" "$dest" 2>/dev/null; then return 0; fi; \
        echo "branch '$ref' not found in $repo; falling back to default branch" >&2; \
        git clone --depth=1 "$url" "$dest"; \
    }; \
    clone_with_fallback accounting-grpc-api "$ACCOUNTING_GRPC_API_REF"; \
    clone_with_fallback payment-util        "$PAYMENT_UTIL_REF"

COPY . /src/clearing-settlement
WORKDIR /src/clearing-settlement

RUN go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/clearing-settlement ./cmd/server

# ── runtime ─────────────────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/clearing-settlement /usr/local/bin/clearing-settlement
COPY config/config.yaml /etc/clearing-settlement/config.yaml
EXPOSE 9890 9891 9892
ENTRYPOINT ["/usr/local/bin/clearing-settlement"]
