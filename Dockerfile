# risk-manage Dockerfile — 自包含构建
#
# 以前这个 Dockerfile 要求 docker build 的 context 是"父目录"，能看到
# ../payment-util。当 outer docker-compose 用 `context: ./risk-manage`
# 这种按仓库隔离的上下文时，sibling COPY 会失败。
#
# 现在改成 Dockerfile 自己 git-clone payment-util 到 /src/payment-util，
# 这样不论 context 是单仓目录还是父目录，Dockerfile 都能工作。
# go.mod 里的 `replace ../payment-util` 依赖的相对路径
# /src/risk-manage → /src/payment-util 仍然成立。
#
# 私仓 clone 需要 token：outer compose 可通过 BuildKit secret 传入：
#   services.risk-manage.build.secrets:
#     - GITHUB_TOKEN
# 然后 GITHUB_TOKEN=ghp_xxx docker compose build；
# 公仓（或本机已登录 GitHub）时不带 secret 也行。
#
# 默认拉 main 分支；CI 想测某条 PR 分支可用 build-arg 覆盖：
#   docker build --build-arg PAYMENT_UTIL_REF=claude/fix-payment-util-pvFNu ...

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
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
        echo "       then run:  GITHUB_TOKEN=ghp_xxx docker compose build" >&2; \
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
    clone_with_fallback payment-util "$PAYMENT_UTIL_REF"

# 这个仓自己的代码
COPY . /src/risk-manage

WORKDIR /src/risk-manage
RUN go mod tidy
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/risk-manage ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/risk-manage /usr/local/bin/
# 构建上下文是 risk-manage 自身目录，config 相对路径就是 config/。
# 整目录拷进去：除了 config.yaml 还有 rules-recipes.yaml（30+ 预置规则）和
# 未来可能加的其它配方文件，loadConfig 期望它们都能在 /etc/risk-manage/ 找到。
COPY config/ /etc/risk-manage/
EXPOSE 9490 9590
ENTRYPOINT ["/usr/local/bin/risk-manage"]
