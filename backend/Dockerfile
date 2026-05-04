# payment-admin-backend 自包含 build
#
# docker build 的 context 是 backend 自己的目录（即本 Dockerfile 所在目录），
# 可以直接 `cd payment-admin-web/backend && docker build .` 或用
# docker-compose.yml `context: ./backend` 触发。
#
# 所有 sibling 仓都由 Dockerfile 自己 git-clone 到 /src/ 下，
# go.mod 里 `replace ../../kms-manage` 相对路径仍然生效：
#   /src/payment-admin-web/backend  →  .. →  /src/payment-admin-web
#                                   →  ../.. →  /src  →  /src/kms-manage
#
# 每个 sibling 的 branch/tag 可以通过 build-arg 覆盖：
#   docker build --build-arg PAYMENT_UTIL_REF=claude/fix-payment-util-pvFNu ...
#
# 私仓 clone 需要 GITHUB_TOKEN（从 BuildKit secret 读）。
FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

# tidy 需要在缺 go.sum 条目时自动补（payment-util 传递进来的 OTel/grpc 等
# indirect 还没 hash），否则 go build 会报 "missing go.sum entry"。
ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto
ENV GOPROXY=https://proxy.golang.org,direct

ARG PAYMENT_UTIL_REF=main
ARG KMS_MANAGE_REF=main
ARG RISK_MANAGE_REF=main
ARG PAYMENT_CHANNEL_REF=main
ARG PAYMENT_CORE_REF=main
ARG ORDER_CORE_REF=main
ARG USER_MERCHANT_CORE_REF=main
ARG ACCOUNTING_GRPC_API_REF=main

RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    TOKEN=""; \
    [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
    if [ -z "$TOKEN" ]; then \
        echo "ERROR: GITHUB_TOKEN is not set; required to clone private xiongwp/* siblings." >&2; \
        echo "       Generate a token at https://github.com/settings/tokens (scope: repo)," >&2; \
        echo "       then run:  GITHUB_TOKEN=ghp_xxx docker compose build" >&2; \
        exit 1; \
    fi; \
    auth="x-access-token:${TOKEN}@"; \
    clone() { \
        repo=$1; ref=$2; \
        url="https://${auth}github.com/xiongwp/${repo}.git"; \
        dest="/src/${repo}"; \
        if git clone --depth=1 --branch "$ref" "$url" "$dest" 2>/dev/null; then \
            return 0; \
        fi; \
        echo "branch '$ref' not found in $repo; falling back to default branch" >&2; \
        git clone --depth=1 "$url" "$dest"; \
    }; \
    clone payment-util         "$PAYMENT_UTIL_REF"; \
    clone kms-manage           "$KMS_MANAGE_REF"; \
    clone risk-manage          "$RISK_MANAGE_REF"; \
    clone payment-channel      "$PAYMENT_CHANNEL_REF"; \
    clone payment-core         "$PAYMENT_CORE_REF"; \
    clone order-core           "$ORDER_CORE_REF"; \
    clone user-merchant-core   "$USER_MERCHANT_CORE_REF"; \
    clone accounting-grpc-api  "$ACCOUNTING_GRPC_API_REF"

# build context 就是 backend 目录；把它完整拷到 /src/payment-admin-web/backend
# 下，go.mod 里所有 `replace ../../<sibling>` 路径都能解到 /src/<sibling>。
COPY . /src/payment-admin-web/backend

WORKDIR /src/payment-admin-web/backend
RUN go mod tidy
RUN go mod download -x
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o /out/payment-admin-backend ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/payment-admin-backend /usr/local/bin/
EXPOSE 9190
ENTRYPOINT ["/usr/local/bin/payment-admin-backend"]
