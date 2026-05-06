# clearing-settlement Dockerfile — 优先用本地兄弟仓，找不到才 git clone。
#
# 默认（推荐）：docker-compose.yml 用 BuildKit additional_contexts 注入本地：
#   build:
#     additional_contexts:
#       payment-util:        ../payment-util
#       accounting-grpc-api: ../accounting-grpc-api
#
# 没有 additional_contexts（CI / 干净 checkout）时，fallback 到 git clone
# （需 BuildKit secret GITHUB_TOKEN）。

# Sibling source provider placeholders — name 与 additional_contexts key 一致，
# 被 docker compose 的 additional_contexts 整体替换。
FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM alpine:3.19 AS accounting-grpc-api
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG ACCOUNTING_GRPC_API_REF=main
ARG PAYMENT_UTIL_REF=main

# Stage sibling sources：override 时拿到真实仓，否则拿到 placeholder（没 go.mod）。
COPY --from=payment-util         / /tmp/payment-util-staged/
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
    promote accounting-grpc-api  "$ACCOUNTING_GRPC_API_REF"

COPY . /src/clearing-settlement
WORKDIR /src/clearing-settlement

# GOPROXY 多源 + 重试：抗 sumdb / 单一 proxy 网络抖动
ENV GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
ENV GOSUMDB=off
RUN for i in 1 2 3 4 5; do \
      go mod tidy && go mod download && break; \
      echo "[go mod] attempt $i failed; retry after 5s..." >&2; \
      sleep 5; \
    done
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/clearing-settlement ./cmd/server

# ── runtime ─────────────────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/clearing-settlement /usr/local/bin/clearing-settlement
COPY config/config.yaml /etc/clearing-settlement/config.yaml
EXPOSE 9890 9891 9892
ENTRYPOINT ["/usr/local/bin/clearing-settlement"]
