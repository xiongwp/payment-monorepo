# reconplatform Dockerfile — 一个 image 装两个 binary：
#   /app/reconplatform   = cmd/main.go         （老 expr 引擎，消费 Kafka 跑规则）
#   /app/recon-admin     = cmd/recon-admin/main.go （新 CDC + 动态对账 + admin web）
#
# 部署方式：override 文件里两个 service 共用本 image，用不同 entrypoint 选 binary。
#
# 兄弟仓 payment-util 通过 BuildKit additional_contexts 注入；其他服务用相同模式。

FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.26-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
COPY --from=payment-util / /tmp/payment-util-staged/

RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    name=payment-util; ref=${PAYMENT_UTIL_REF}; \
    staged="/tmp/${name}-staged"; final="/src/${name}"; \
    if [ -f "${staged}/go.mod" ]; then \
      echo "→ Using local ${name}"; \
      mkdir -p "${final}"; cp -a "${staged}/." "${final}/"; \
    else \
      TOKEN=""; [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
      [ -z "$TOKEN" ] && { echo "ERROR: provide ../${name} or GITHUB_TOKEN" >&2; exit 1; }; \
      url="https://x-access-token:${TOKEN}@github.com/xiongwp/${name}.git"; \
      rm -rf "${final}"; \
      git clone --depth=1 --branch "$ref" "$url" "$final" 2>/dev/null \
        || git clone --depth=1 "$url" "$final"; \
    fi; \
    rm -rf "${staged}"

# 本仓代码
COPY . /src/reconplatform

WORKDIR /src/reconplatform
ENV GOPROXY=https://proxy.golang.org,https://goproxy.cn,direct
ENV GOSUMDB=off
ENV GOINSECURE=goproxy.cn
RUN for i in 1 2 3 4 5; do go mod tidy && go mod download && exit 0; echo "[go mod] attempt $i" >&2; sleep 5; done; exit 1

# 编 2 个 binary：
#   reconplatform → cmd/main.go (老 engine)
#   recon-admin   → cmd/recon-admin/main.go (新 admin)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/reconplatform ./cmd
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/recon-admin ./cmd/recon-admin

# ─── runtime ────────────────────────────────────────────────────
FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /out/reconplatform /app/reconplatform
COPY --from=builder /out/recon-admin   /app/recon-admin
# configs/cdc.sources.yaml — recon-admin 在 config-center 不可达 (dev) 时用作 fallback
COPY --from=builder /src/reconplatform/configs /app/configs
USER app
EXPOSE 8080

# 默认 CMD 跑老 engine；admin 容器在 compose 里 entrypoint 覆盖跑 /app/recon-admin。
CMD ["/app/reconplatform"]
