# kms-manage Dockerfile — 优先用本地 ../payment-util，找不到才 git clone。
#
# 默认（推荐）：docker-compose.yml 用 BuildKit additional_contexts 注入：
#   build:
#     additional_contexts:
#       payment-util: ../payment-util
#
# 没设置时 fallback 到 git clone（需 GITHUB_TOKEN secret）。

# Sibling source provider placeholder。
FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.25-alpine AS build

RUN apk add --no-cache git ca-certificates

WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main

# cache-buster：兄弟仓内容变更但 BuildKit 缓存了旧 staged 时改 v=N
ARG SIBLING_CONTEXT_VER=1
RUN echo "sibling-ctx ver=${SIBLING_CONTEXT_VER}"

COPY --from=payment-util / /tmp/payment-util-staged/

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
    promote payment-util "$PAYMENT_UTIL_REF"

# 本仓代码 → /src/kms-manage（与 /src/payment-util 同级，匹配 go.mod 里的 ../payment-util）
COPY . /src/kms-manage
WORKDIR /src/kms-manage
# GOPROXY 多源 + 重试：抗 sumdb / 单一 proxy 网络抖动 (storage.googleapis.com EOF 等)
ENV GOPROXY=https://proxy.golang.org,https://goproxy.cn,direct
ENV GOSUMDB=off
ENV GOINSECURE=goproxy.cn
RUN for i in 1 2 3 4 5; do go mod tidy && go mod download && exit 0; echo "[go mod] attempt $i failed; retry after 5s..." >&2; sleep 5; done; echo "[go mod] all 5 attempts failed; aborting build" >&2; exit 1
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/kms-manage ./cmd/server
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/kmsctl ./cmd/kmsctl

# --- runtime ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/kms-manage /usr/local/bin/
COPY --from=build /out/kmsctl     /usr/local/bin/
# 构建上下文是 kms-manage 自身目录，config 相对路径就是 config/
COPY config/config.yaml /etc/kms-manage/config.yaml
EXPOSE 9290 9390
ENTRYPOINT ["/usr/local/bin/kms-manage"]
