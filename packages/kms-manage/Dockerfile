# kms-manage Dockerfile — 自包含构建
#
# 以前要求 docker build context 是"父目录"以看到 ../payment-util。
# 当 outer docker-compose 用 `context: ./kms-manage` 时，sibling COPY 会失败。
#
# 现在 Dockerfile 自己 git-clone payment-util 到 /src/payment-util，
# go.mod 里的 `replace ../payment-util` 依赖的相对路径仍然成立。
#
# 私仓 clone 通过 BuildKit secret 传 token：
#   services.kms.build.secrets:
#     - GITHUB_TOKEN
# 然后 GITHUB_TOKEN=ghp_xxx docker compose build；
# 公仓不带 secret 也行。
#
# 默认拉 main；覆盖：--build-arg PAYMENT_UTIL_REF=<branch>

# --- build ---
# 自包含 build：BuildKit secret 拿 GITHUB_TOKEN，自己 git-clone payment-util
# 到 /src/payment-util，让 go.mod 里的 `replace ../payment-util` 在容器内能命中。
FROM golang:1.25-alpine AS build

RUN apk add --no-cache git ca-certificates

WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
ARG CACHEBUST=0
RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    set -eu; \
    echo "cachebust=$CACHEBUST"; \
    TOKEN=""; \
    [ -f /run/secrets/GITHUB_TOKEN ] && TOKEN="$(cat /run/secrets/GITHUB_TOKEN)"; \
    if [ -z "$TOKEN" ]; then \
        echo "ERROR: GITHUB_TOKEN is required to clone xiongwp/payment-util." >&2; \
        echo "       Generate at https://github.com/settings/tokens (scope: repo)," >&2; \
        echo "       then run:  GITHUB_TOKEN=ghp_xxx docker compose build" >&2; \
        exit 1; \
    fi; \
    auth="x-access-token:${TOKEN}@"; \
    url="https://${auth}github.com/xiongwp/payment-util.git"; \
    if git clone --depth=1 --branch "$PAYMENT_UTIL_REF" "$url" /src/payment-util 2>/dev/null; then \
        :; \
    else \
        echo "branch '$PAYMENT_UTIL_REF' not found in payment-util; falling back to default branch" >&2; \
        git clone --depth=1 "$url" /src/payment-util; \
    fi

# 本仓代码 → /src/kms-manage（与 /src/payment-util 同级，匹配 go.mod 里的 ../payment-util）
COPY . /src/kms-manage
WORKDIR /src/kms-manage
RUN go mod tidy
RUN go mod download
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
