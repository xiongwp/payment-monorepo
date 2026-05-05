# card-payment Dockerfile —— 跟 card-center / user-merchant-core 同模板
FROM alpine:3.19 AS payment-util
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder
# alias `card-center-src`：避免跟 payment-stack 网络里的 card-center:local image 撞名
# （撞名会让 buildx 把 additional_context 解析为 docker-image:// 而不是本地路径）。
FROM alpine:3.19 AS card-center-src
RUN mkdir -p /sibling-placeholder && echo "no-go.mod-here" > /sibling-placeholder/.placeholder

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src
ENV GOFLAGS=-mod=mod
ENV GOTOOLCHAIN=auto

ARG PAYMENT_UTIL_REF=main
ARG CARD_CENTER_REF=main

# cache-buster
<<<<<<< HEAD
ARG SIBLING_CONTEXT_VER=3
=======
ARG SIBLING_CONTEXT_VER=99
>>>>>>> feat/shadow-traffic
RUN echo "sibling-ctx ver=${SIBLING_CONTEXT_VER}"

COPY --from=payment-util    / /tmp/payment-util-staged/
COPY --from=card-center-src / /tmp/card-center-staged/

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
        if [ -z "$TOKEN" ]; then echo "ERROR: provide ../${name} or token" >&2; exit 1; fi; \
        url="https://x-access-token:${TOKEN}@github.com/xiongwp/${name}.git"; \
        rm -rf "${final}"; \
        git clone --depth=1 --branch "$ref" "$url" "$final" 2>/dev/null || git clone --depth=1 "$url" "$final"; \
      fi; \
      rm -rf "${staged}"; \
    }; \
    promote payment-util "$PAYMENT_UTIL_REF"; \
    promote card-center  "$CARD_CENTER_REF"

COPY . /src/card-payment

WORKDIR /src/card-payment
RUN go mod tidy
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/card-payment ./cmd/server

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/card-payment /app/card-payment
COPY config /app/config
USER app
EXPOSE 9443 9543
CMD ["/app/card-payment"]
