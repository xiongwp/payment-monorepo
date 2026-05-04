# payment-admin-backend — 优先用本地兄弟仓，找不到才 git clone。
#
# 默认（推荐）：docker-compose.yml 用 BuildKit additional_contexts 注入 8 个本地仓：
#   build:
#     additional_contexts:
#       payment-util:        ../../payment-util
#       kms-manage:          ../../kms-manage
#       risk-manage:         ../../risk-manage
#       payment-channel:     ../../payment-channel
#       payment-core:        ../../payment-core
#       order-core:          ../../order-core
#       user-merchant-core:  ../../user-merchant-core
#       accounting-grpc-api: ../../accounting-grpc-api
# 注意 backend 在 payment-admin-web/backend 子目录下，故 ../../ 才到 packages/ 根。
#
# 没设置时 fallback 到 git clone（需 GITHUB_TOKEN secret）。

# Sibling source provider placeholders。
FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS kms-manage
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS risk-manage
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS payment-channel
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS payment-core
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS order-core
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS user-merchant-core
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
FROM alpine:3.19 AS accounting-grpc-api
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

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

COPY --from=payment-util         / /tmp/payment-util-staged/
COPY --from=kms-manage           / /tmp/kms-manage-staged/
COPY --from=risk-manage          / /tmp/risk-manage-staged/
COPY --from=payment-channel      / /tmp/payment-channel-staged/
COPY --from=payment-core         / /tmp/payment-core-staged/
COPY --from=order-core           / /tmp/order-core-staged/
COPY --from=user-merchant-core   / /tmp/user-merchant-core-staged/
COPY --from=accounting-grpc-api  / /tmp/accounting-grpc-api-staged/

RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    promote() { \
      name=$1; ref=$2; \
      staged="/tmp/${name}-staged"; final="/src/${name}"; \
      if [ -f "${staged}/go.mod" ]; then \
        echo "→ Using local ${name} (additional_context override)"; \
        mkdir -p "${final}"; cp -a "${staged}/." "${final}/"; \
      else \
        echo "→ Local ${name} not provided, git clone fallback (ref=${ref})"; \
        TOKEN=""; \
        [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
        if [ -z "$TOKEN" ]; then \
          echo "ERROR: cannot fetch sibling '${name}' — provide ../${name} via additional_contexts OR GITHUB_TOKEN secret." >&2; \
          exit 1; \
        fi; \
        auth="x-access-token:${TOKEN}@"; \
        url="https://${auth}github.com/xiongwp/${name}.git"; \
        rm -rf "${final}"; \
        git clone --depth=1 --branch "$ref" "$url" "$final" 2>/dev/null \
          || git clone --depth=1 "$url" "$final"; \
      fi; \
      rm -rf "${staged}"; \
    }; \
    promote payment-util         "$PAYMENT_UTIL_REF"; \
    promote kms-manage           "$KMS_MANAGE_REF"; \
    promote risk-manage          "$RISK_MANAGE_REF"; \
    promote payment-channel      "$PAYMENT_CHANNEL_REF"; \
    promote payment-core         "$PAYMENT_CORE_REF"; \
    promote order-core           "$ORDER_CORE_REF"; \
    promote user-merchant-core   "$USER_MERCHANT_CORE_REF"; \
    promote accounting-grpc-api  "$ACCOUNTING_GRPC_API_REF"

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
