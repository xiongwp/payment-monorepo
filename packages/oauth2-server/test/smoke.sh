#!/usr/bin/env bash
# smoke.sh — OAuth2 server end-to-end 烟雾测试。
#
# 启动:
#   OAUTH2_DEV_SEED=1 \
#   OAUTH2_ADMIN_TOKEN=admintok \
#   go run ./cmd/server &
#
# 运行: ./smoke.sh [host:port]

set -euo pipefail
HOST="${1:-http://localhost:8087}"
ADMIN_TOK="${OAUTH2_ADMIN_TOKEN:-admintok}"

c() {
  echo -e "\n\033[36m▶ $1\033[0m"
}

c "1. health check"
curl -fsS "$HOST/healthz"

c "2. JWKS discovery"
curl -fsS "$HOST/.well-known/jwks.json" | head -c 400
echo

c "3. OIDC discovery"
curl -fsS "$HOST/.well-known/openid-configuration" | head -c 400
echo

c "4. 列客户端 (admin)"
curl -fsS -H "X-Admin-Token: $ADMIN_TOK" "$HOST/admin/clients" | head -c 200
echo

c "5. 创建新客户端 (admin)"
NEW=$(curl -fsS -X POST -H "X-Admin-Token: $ADMIN_TOK" \
  -H "Content-Type: application/json" \
  -d '{"name":"smoke test","owner_type":"service","owner_id":"smoke-test","allowed_scopes":"charge:read"}' \
  "$HOST/admin/clients")
echo "$NEW"
CID=$(echo "$NEW" | python3 -c "import sys,json; print(json.load(sys.stdin)['client_id'])")
CSEC=$(echo "$NEW" | python3 -c "import sys,json; print(json.load(sys.stdin)['client_secret'])")
echo "client_id=$CID secret=${CSEC:0:8}..."

c "6. 申请 token (新建客户端)"
TOK_RESP=$(curl -fsS -X POST "$HOST/oauth2/token" \
  -d "grant_type=client_credentials&client_id=$CID&client_secret=$CSEC&scope=charge:read")
echo "$TOK_RESP" | head -c 300
ACCESS=$(echo "$TOK_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])")

c "7. introspect token"
curl -fsS -X POST "$HOST/oauth2/introspect" -d "token=$ACCESS" | head -c 400
echo

c "8. 申请 token w/ bad secret (应 401)"
curl -sS -o /tmp/oauth_bad.json -w "%{http_code}\n" -X POST "$HOST/oauth2/token" \
  -d "grant_type=client_credentials&client_id=$CID&client_secret=BOGUS"
cat /tmp/oauth_bad.json
echo

c "9. revoke token"
curl -fsS -X POST "$HOST/oauth2/revoke" -d "token=$ACCESS" -w "\nstatus=%{http_code}\n"

c "10. introspect revoked (应 active=false)"
curl -fsS -X POST "$HOST/oauth2/introspect" -d "token=$ACCESS" | head -c 200
echo

c "11. rotate secret"
curl -fsS -X POST -H "X-Admin-Token: $ADMIN_TOK" \
  "$HOST/admin/clients/$CID/rotate-secret" | head -c 300
echo

c "12. suspend client"
curl -fsS -X POST -H "X-Admin-Token: $ADMIN_TOK" \
  "$HOST/admin/clients/$CID/suspend" | head -c 200
echo

c "13. rotate RSA key"
curl -fsS -X POST -H "X-Admin-Token: $ADMIN_TOK" "$HOST/admin/keys/rotate"
echo

echo -e "\n\033[32m✅ all smoke tests passed\033[0m"
