# config-center Dockerfile —— 跟 card-center / kms-manage 同模板。
# 优先用本地兄弟仓 additional_contexts 注入；找不到 fallback git clone。
#
# 兄弟仓：
#   payment-util         给 trace / shadow / serviceregistry / mtls helpers 用
#   user-merchant-core   admin 真鉴权调 IntrospectToken 用（P0-6 引入）

FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM alpine:3.19 AS user-merchant-core
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
ARG USER_MERCHANT_CORE_REF=main
ARG SIBLING_CONTEXT_VER=2
RUN echo "sibling-ctx ver=${SIBLING_CONTEXT_VER}"

COPY --from=payment-util / /tmp/payment-util-staged/
COPY --from=user-merchant-core / /tmp/user-merchant-core-staged/

RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    promote() { \
      local name="$1"; local ref="$2"; \
      local staged="/tmp/${name}-staged"; local final="/src/${name}"; \
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
    promote payment-util       "${PAYMENT_UTIL_REF}"; \
    promote user-merchant-core "${USER_MERCHANT_CORE_REF}"

COPY . /src/config-center

WORKDIR /src/config-center
# GOPROXY 多源 + 重试：抗 sumdb / 单一 proxy 网络抖动 (storage.googleapis.com EOF 等)
ENV GOPROXY=https://proxy.golang.org,https://goproxy.cn,direct
ENV GOSUMDB=off
ENV GOINSECURE=goproxy.cn
RUN for i in 1 2 3 4 5; do go mod tidy && go mod download && exit 0; echo "[go mod] attempt $i failed; retry after 5s..." >&2; sleep 5; done; echo "[go mod] all 5 attempts failed; aborting build" >&2; exit 1
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/config-center ./cmd/server

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/config-center /app/config-center
COPY config /app/config
COPY database /app/database
USER app
EXPOSE 9690 9691 9692
CMD ["/app/config-center"]
