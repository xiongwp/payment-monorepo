# api-gateway Dockerfile — 自包含构建（与 payment-core 风格一致）
#
# go.mod 通过 `replace ../<sibling>` 引用同级仓。容器内 git-clone 进 /src/<name>
# 让 replace 路径成立。token 通过 BuildKit secret 传：
#
#   GITHUB_TOKEN=ghp_xxx docker build --secret id=GITHUB_TOKEN,env=GITHUB_TOKEN .
#
# 覆盖 sibling 分支：--build-arg PAYMENT_UTIL_REF / KMS_MANAGE_REF。

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
ARG KMS_MANAGE_REF=main
ARG USER_MERCHANT_CORE_REF=main
ARG RISK_MANAGE_REF=main

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
        echo "       Generate a token at https://github.com/settings/tokens (scope: repo)," >&2; \
        echo "       then run:  GITHUB_TOKEN=ghp_xxx docker build --secret id=GITHUB_TOKEN,env=GITHUB_TOKEN ." >&2; \
        exit 1; \
    fi; \
    auth="x-access-token:${TOKEN}@"; \
    clone_with_fallback() { \
        repo=$1; ref=$2; \
        url="https://${auth}github.com/xiongwp/${repo}.git"; \
        dest="/src/${repo}"; \
        if git clone --depth=1 --branch "$ref" "$url" "$dest" 2>/dev/null; then \
            return 0; \
        fi; \
        echo "branch '$ref' not found in $repo; falling back to default branch" >&2; \
        git clone --depth=1 "$url" "$dest"; \
    }; \
    clone_with_fallback payment-util "$PAYMENT_UTIL_REF"; \
    clone_with_fallback kms-manage          "$KMS_MANAGE_REF"; \
    clone_with_fallback user-merchant-core  "$USER_MERCHANT_CORE_REF"; \
    clone_with_fallback risk-manage         "$RISK_MANAGE_REF"

COPY . /src/api-gateway
WORKDIR /src/api-gateway

RUN go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api-gateway ./cmd/server

# ── runtime ─────────────────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/api-gateway /usr/local/bin/api-gateway
COPY config/config.yaml /etc/api-gateway/config.yaml
EXPOSE 8080 8081 9090
ENTRYPOINT ["/usr/local/bin/api-gateway"]
