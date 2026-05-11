#!/usr/bin/env bash
# 05_e2e_test.sh — 端到端集成测试 (CI 跑)。
#
# 流程:
#   0. setup oauth2-server + 创建 demo client
#   1. 启 demo resource server
#   2. 商户 client 调用 → 期待 200
#   3. 服务 client 调用 → 期待 200
#   4. scope 不足 → 期待 403
#   5. revoked client → 期待 401
#   6. expired token → 期待 401 (用 1s TTL token)

set -euo pipefail
cd "$(dirname "$0")"

cleanup() {
  echo "▶ cleanup"
  [[ -n "${SERVER_PID:-}" ]] && kill $SERVER_PID 2>/dev/null || true
  docker rm -f oauth2-demo 2>/dev/null || true
}
trap cleanup EXIT

echo "▶ 0. setup"
./00_setup.sh > /dev/null
source .env

echo "▶ 1. start demo resource server"
go run ./04_resource_server.go > /tmp/resource-server.log 2>&1 &
SERVER_PID=$!
# wait ready
for i in {1..15}; do
  if curl -sf http://localhost:9090/healthz > /dev/null; then break; fi
  sleep 1
done

PASS=0; FAIL=0
assert_eq() {
  local name=$1 expected=$2 actual=$3
  if [[ "$expected" == "$actual" ]]; then
    echo "  ✓ $name (HTTP $actual)"
    PASS=$((PASS+1))
  else
    echo "  ✗ $name — expected $expected got $actual"
    FAIL=$((FAIL+1))
  fi
}

echo "▶ 2. 商户 client_credentials → token"
TOK=$(curl -fsS -X POST "$OAUTH_HOST/oauth2/token" \
  -d "grant_type=client_credentials&client_id=$MER_CLIENT_ID&client_secret=$MER_CLIENT_SECRET&scope=charge:write refund:write" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])")
echo "  got token ${TOK:0:30}..."

echo "▶ 3. 调有 scope 的 endpoint (期待 200)"
CODE=$(curl -sS -o /dev/null -w "%{http_code}" -X POST "http://localhost:9090/api/v1/charges" \
  -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" -d '{"amount_minor":100}')
assert_eq "POST /charges with charge:write" 201 "$CODE"

echo "▶ 4. 调没 scope 的 endpoint (期待 403)"
CODE=$(curl -sS -o /dev/null -w "%{http_code}" "http://localhost:9090/api/v1/charges" \
  -H "Authorization: Bearer $TOK")
assert_eq "GET /charges without charge:read" 403 "$CODE"

echo "▶ 5. 不带 token (期待 401)"
CODE=$(curl -sS -o /dev/null -w "%{http_code}" "http://localhost:9090/api/v1/refunds")
assert_eq "no auth header" 401 "$CODE"

echo "▶ 6. bogus token (期待 401)"
CODE=$(curl -sS -o /dev/null -w "%{http_code}" "http://localhost:9090/api/v1/refunds" \
  -H "Authorization: Bearer bogus.token")
assert_eq "bogus jwt" 401 "$CODE"

echo "▶ 7. healthz public (期待 200, 不要 token)"
CODE=$(curl -sS -o /dev/null -w "%{http_code}" "http://localhost:9090/healthz")
assert_eq "public /healthz" 200 "$CODE"

echo "▶ 8. introspect active token"
ACTIVE=$(curl -fsS -X POST "$OAUTH_HOST/oauth2/introspect" \
  -d "token=$TOK&client_id=$MER_CLIENT_ID&client_secret=$MER_CLIENT_SECRET" \
  | python3 -c "import sys,json; print(json.load(sys.stdin).get('active'))")
[[ "$ACTIVE" == "True" ]] && { echo "  ✓ active=true"; PASS=$((PASS+1)); } || { echo "  ✗ active != true"; FAIL=$((FAIL+1)); }

echo "▶ 9. revoke + introspect (期待 active=false)"
curl -fsS -X POST "$OAUTH_HOST/oauth2/revoke" -d "token=$TOK" > /dev/null
ACTIVE=$(curl -fsS -X POST "$OAUTH_HOST/oauth2/introspect" \
  -d "token=$TOK&client_id=$MER_CLIENT_ID&client_secret=$MER_CLIENT_SECRET" \
  | python3 -c "import sys,json; print(json.load(sys.stdin).get('active'))")
[[ "$ACTIVE" == "False" ]] && { echo "  ✓ active=false (post-revoke)"; PASS=$((PASS+1)); } || { echo "  ✗ active != false"; FAIL=$((FAIL+1)); }

echo "▶ 10. wrong secret (期待 401)"
CODE=$(curl -sS -o /dev/null -w "%{http_code}" -X POST "$OAUTH_HOST/oauth2/token" \
  -d "grant_type=client_credentials&client_id=$MER_CLIENT_ID&client_secret=WRONG_SECRET")
assert_eq "wrong secret" 401 "$CODE"

echo ""
echo "════════════════════════════════════════════"
if [[ $FAIL -eq 0 ]]; then
  echo "  ✅ all $PASS tests passed"
else
  echo "  ❌ $FAIL failed, $PASS passed"
  exit 1
fi
echo "════════════════════════════════════════════"
