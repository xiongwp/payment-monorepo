#!/usr/bin/env bash
# 03_curl_flow.sh — 纯 curl 复现完整 OAuth2 流程 (调试 / 非 Go 服务接入参考)。
#
# 跑完 00_setup.sh 后 source .env 再跑此脚本。

set -euo pipefail
[[ -f .env ]] && source .env

OAUTH_HOST="${OAUTH_HOST:-http://localhost:8087}"
API_BASE="${API_BASE:-http://localhost:9090}"

c() { echo -e "\n\033[1;36m▶ $1\033[0m"; }
g() { echo -e "\033[32m  $1\033[0m"; }

c "1. 拿 token (client_credentials)"
TOK_RESP=$(curl -fsS -X POST "$OAUTH_HOST/oauth2/token" \
  -d "grant_type=client_credentials" \
  -d "client_id=$MER_CLIENT_ID" \
  -d "client_secret=$MER_CLIENT_SECRET" \
  -d "scope=charge:write refund:write")
echo "$TOK_RESP" | python3 -m json.tool
ACCESS=$(echo "$TOK_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])")

c "2. 解 JWT header + claims (base64url decode)"
HEADER=$(echo "$ACCESS" | cut -d. -f1)
PAYLOAD=$(echo "$ACCESS" | cut -d. -f2)
# base64 padding fix
pad() { printf "%s%s" "$1" "$(printf '=%.0s' $(seq 1 $((4 - ${#1} % 4))))"; }
g "Header:"
echo "$(pad $HEADER)" | tr '_-' '/+' | base64 -d 2>/dev/null | python3 -m json.tool
g "Claims:"
echo "$(pad $PAYLOAD)" | tr '_-' '/+' | base64 -d 2>/dev/null | python3 -m json.tool

c "3. 调 resource server API (带 Bearer)"
curl -sS -X POST "$API_BASE/api/v1/charges" \
  -H "Authorization: Bearer $ACCESS" \
  -H "Content-Type: application/json" \
  -d '{"amount_minor":100,"currency":"USD"}' | python3 -m json.tool

c "4. 同 token 调多个 endpoint (验证 token 可复用直到 exp)"
for ep in charges refunds whoami; do
  g "/api/v1/$ep:"
  curl -sS "$API_BASE/api/v1/$ep" -H "Authorization: Bearer $ACCESS" \
    | python3 -c "import sys,json; d=json.load(sys.stdin); print(' ',json.dumps(d)[:200])"
done

c "5. 不带 token → 401"
curl -sS -o /dev/null -w "  HTTP %{http_code}\n" "$API_BASE/api/v1/refunds"

c "6. 假 token → 401"
curl -sS -o /dev/null -w "  HTTP %{http_code}\n" \
  "$API_BASE/api/v1/refunds" -H "Authorization: Bearer bogus.token.here"

c "7. 用 introspect 验签 (非 Go 服务用这个最简单)"
curl -sS -X POST "$OAUTH_HOST/oauth2/introspect" \
  -d "token=$ACCESS" \
  -d "client_id=$MER_CLIENT_ID" \
  -d "client_secret=$MER_CLIENT_SECRET" | python3 -m json.tool

c "8. revoke + introspect 再看 (应 active=false)"
curl -fsS -X POST "$OAUTH_HOST/oauth2/revoke" -d "token=$ACCESS" -w "\n  revoked, status=%{http_code}\n"
curl -sS -X POST "$OAUTH_HOST/oauth2/introspect" -d "token=$ACCESS" | python3 -m json.tool

c "9. 用 revoked token 调 API"
curl -sS -o /dev/null -w "  HTTP %{http_code}\n" "$API_BASE/api/v1/refunds" \
  -H "Authorization: Bearer $ACCESS"
echo "  (注: payment-mw 现在不查 revocation; 要加可在 mw.Auth 后接 introspect。"
echo "       生产推荐配 1min LRU 缓存的 introspect call 来防 revoked token.)"

echo -e "\n\033[1;32m✅ curl flow 完成\033[0m"
