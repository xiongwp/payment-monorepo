# id-generator Dockerfile — 优先用本地 ../payment-util，找不到才 git clone。
#
# 默认（推荐）：docker-compose.yml 用 BuildKit additional_contexts 注入：
#   build:
#     additional_contexts:
#       payment-util: ../payment-util
#
# 没设置时 fallback 到 git clone（需 GITHUB_TOKEN secret）。
#
# go.mod 里的 `replace github.com/xiongwp/payment-util => ../payment-util` 仍然生效：
# 容器内把 payment-util 放到 /src/payment-util，跟 /src/id-generator 兄弟，
# 相对路径吻合。

# Sibling source provider placeholder。
FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.26.1 AS builder

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main

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

# 这个仓自己的代码 → /src/id-generator，跟 /src/payment-util 兄弟。
COPY . /src/id-generator
WORKDIR /src/id-generator

# GOPROXY 多源 + 重试：抗 sumdb / 单一 proxy 网络抖动
ENV GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
ENV GOSUMDB=off
RUN for i in 1 2 3 4 5; do \
      go mod tidy && go mod download && break; \
      echo "[go mod] attempt $i failed; retry after 5s..." >&2; \
      sleep 5; \
    done
RUN CGO_ENABLED=0 go build -o /out/id-server ./cmd/server

# 运行镜像（更小）
FROM debian:bookworm-slim

WORKDIR /app
COPY --from=builder /out/id-server .

CMD ["./id-server"]
