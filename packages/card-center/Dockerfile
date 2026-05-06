# card-center Dockerfile —— 跟 user-merchant-core 同模板。
# 优先用本地兄弟仓 additional_contexts 注入；找不到 fallback git clone。
#
# 兄弟仓：
#   payment-util        给 trace / shadow / serviceregistry helpers 用
#   user-merchant-core  给 internal/httpsauth Verifier 引 IntrospectToken proto stub 用
#   kms-manage          给 internal/kmsclient 引 KMSService gRPC stub 用

FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS user-merchant-core-src
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS kms-manage-src
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
ARG USER_MERCHANT_CORE_REF=main
ARG KMS_MANAGE_REF=main

# cache-buster：兄弟仓内容变更但 BuildKit 还缓存了旧 staged 时改 v=N
ARG SIBLING_CONTEXT_VER=6
RUN echo "sibling-ctx ver=${SIBLING_CONTEXT_VER}"

COPY --from=payment-util            / /tmp/payment-util-staged/
COPY --from=user-merchant-core-src  / /tmp/user-merchant-core-staged/
COPY --from=kms-manage-src          / /tmp/kms-manage-staged/

RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    promote() { \
      name=$1; ref=$2; \
      staged="/tmp/${name}-staged"; final="/src/${name}"; \
      if [ -f "${staged}/go.mod" ]; then \
        echo "→ Using local ${name}"; \
        mkdir -p "${final}"; cp -a "${staged}/." "${final}/"; \
      else \
        TOKEN=""; \
        [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
        if [ -z "$TOKEN" ]; then \
          echo "ERROR: provide ../${name} or GITHUB_TOKEN" >&2; exit 1; \
        fi; \
        auth="x-access-token:${TOKEN}@"; \
        url="https://${auth}github.com/xiongwp/${name}.git"; \
        rm -rf "${final}"; \
        git clone --depth=1 --branch "$ref" "$url" "$final" 2>/dev/null \
          || git clone --depth=1 "$url" "$final"; \
      fi; \
      rm -rf "${staged}"; \
    }; \
    promote payment-util        "$PAYMENT_UTIL_REF"; \
    promote user-merchant-core  "$USER_MERCHANT_CORE_REF"; \
    promote kms-manage          "$KMS_MANAGE_REF"

COPY . /src/card-center

WORKDIR /src/card-center
RUN go mod tidy
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/card-center ./cmd/server

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/card-center /app/card-center
COPY config /app/config
USER app
EXPOSE 9443 9543
CMD ["/app/card-center"]
