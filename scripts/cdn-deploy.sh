#!/usr/bin/env bash
# cdn-deploy.sh — 前端构建 + 上传 S3/R2 + Cloudflare 缓存失效。
#
# CI 跑:
#   ./cdn-deploy.sh packages/payment-admin-web/frontend
#   ./cdn-deploy.sh packages/biz-admin-web/dist
#
# 关键设计:
#   1. 文件名带 content-hash (vite/webpack 自动加) → 不需要 invalidate
#   2. index.html / *.html 短 cache, 部署后立即 purge
#   3. 失败 → exit non-zero, CI 阻塞

set -euo pipefail

SRC_DIR="${1:?usage: $0 <src-dir>}"
S3_BUCKET="${S3_BUCKET:-payment-static-prod}"
S3_PREFIX="${S3_PREFIX:-}"
CF_ZONE_ID="${CLOUDFLARE_ZONE_ID:?need CLOUDFLARE_ZONE_ID}"
CF_TOKEN="${CLOUDFLARE_API_TOKEN:?need CLOUDFLARE_API_TOKEN}"
DOMAIN="${DOMAIN:-static.payment.example.com}"

ts() { date -u +'%Y-%m-%dT%H:%M:%SZ'; }
say() { echo "[$(ts)] $1"; }

cd "$SRC_DIR"
say "▶ Source: $SRC_DIR ($(find . -type f | wc -l) files)"

# 1. 文件分类: 不可变 (有 hash) vs 可变 (index.html)
say "▶ Upload immutable assets (long cache)"
# CSS/JS/字体/图片含 hash 模式 (vite 默认: *.[hash].js)
aws s3 sync . "s3://$S3_BUCKET/$S3_PREFIX" \
  --exclude "*" \
  --include "*.[a-f0-9]*.js" --include "*.[a-f0-9]*.css" \
  --include "*.woff2" --include "*.png" --include "*.svg" --include "*.webp" \
  --cache-control "public,max-age=31536000,immutable" \
  --metadata-directive REPLACE

say "▶ Upload mutable HTML/JSON (short cache)"
aws s3 sync . "s3://$S3_BUCKET/$S3_PREFIX" \
  --exclude "*" --include "*.html" --include "*.json" \
  --cache-control "public,max-age=60,s-maxage=300" \
  --metadata-directive REPLACE

# 2. 失效 Cloudflare 缓存 (只 purge index.html / api 描述文件)
say "▶ Purge Cloudflare cache"
PURGE_URLS=$(find . -name "*.html" -o -name "manifest.json" -o -name "openapi.yaml" | sed "s|^\./|https://$DOMAIN/|")
JSON_FILES=$(echo "$PURGE_URLS" | python3 -c "
import sys, json
urls = [l for l in sys.stdin.read().splitlines() if l.strip()]
print(json.dumps({'files': urls}))
")
curl -fsS -X POST "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/purge_cache" \
  -H "Authorization: Bearer $CF_TOKEN" \
  -H "Content-Type: application/json" \
  -d "$JSON_FILES" | python3 -m json.tool

# 3. smoke test (cache 应该 miss 1 次然后命中)
say "▶ Smoke test cache"
for url in $(echo "$PURGE_URLS" | head -3); do
  RESP=$(curl -sI "$url" | grep -iE "cf-cache-status|cache-control" | head -2)
  echo "  $url"
  echo "$RESP" | sed 's/^/    /'
done

say "✅ CDN deploy done"
