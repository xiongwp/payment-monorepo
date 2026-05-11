#!/usr/bin/env bash
# 00_setup.sh — 起 oauth2-server + 创建 demo client。
#
# 前置:
#   - oauth2-server:local 镜像已 build (cd packages/oauth2-server && docker build -t oauth2-server:local .)

set -euo pipefail

OAUTH_HOST="${OAUTH_HOST:-http://localhost:8087}"
ADMIN_TOK="${OAUTH_ADMIN_TOK:-admintok}"

echo "▶ 1. 启 oauth2-server (Docker)"
docker rm -f oauth2-demo 2>/dev/null || true
docker run -d --name oauth2-demo \
  -p 8087:8087 \
  -e OAUTH2_DEV_SEED=1 \
  -e OAUTH2_ADMIN_TOKEN="$ADMIN_TOK" \
  -e OAUTH2_ISSUER="$OAUTH_HOST" \
  -e OAUTH2_AUDIENCE=payment-api \
  -e OAUTH2_TOKEN_TTL_SEC=3600 \
  oauth2-server:local

echo "▶ 2. 等 ready"
for i in {1..30}; do
  if curl -sf "$OAUTH_HOST/healthz" > /dev/null; then echo "  ok"; break; fi
  sleep 1
done

echo "▶ 3. 创建商户 demo client"
MERCHANT=$(curl -fsS -X POST -H "X-Admin-Token: $ADMIN_TOK" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Demo Merchant Inc.",
    "owner_type": "merchant",
    "owner_id": "merchant_demo_001",
    "allowed_scopes": "charge:write charge:read refund:write refund:read"
  }' \
  "$OAUTH_HOST/admin/clients")
echo "$MERCHANT" | python3 -m json.tool

MER_CID=$(echo "$MERCHANT" | python3 -c "import sys,json; print(json.load(sys.stdin)['client_id'])")
MER_SEC=$(echo "$MERCHANT" | python3 -c "import sys,json; print(json.load(sys.stdin)['client_secret'])")

echo "▶ 4. 创建内部服务 demo client"
SERVICE=$(curl -fsS -X POST -H "X-Admin-Token: $ADMIN_TOK" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "payment-gateway (internal)",
    "owner_type": "service",
    "owner_id": "payment-gateway",
    "allowed_scopes": "charge:write refund:write refund:read dispute:read"
  }' \
  "$OAUTH_HOST/admin/clients")
SVC_CID=$(echo "$SERVICE" | python3 -c "import sys,json; print(json.load(sys.stdin)['client_id'])")
SVC_SEC=$(echo "$SERVICE" | python3 -c "import sys,json; print(json.load(sys.stdin)['client_secret'])")

echo ""
echo "▶ 5. 导出到 .env (运行 demo 用)"
cat > .env <<EOF
OAUTH_HOST=$OAUTH_HOST

# 商户
MER_CLIENT_ID=$MER_CID
MER_CLIENT_SECRET=$MER_SEC

# 内部服务
SVC_CLIENT_ID=$SVC_CID
SVC_CLIENT_SECRET=$SVC_SEC

# resource server config
OAUTH2_JWKS_URL=$OAUTH_HOST/.well-known/jwks.json
OAUTH2_ISSUER=$OAUTH_HOST
OAUTH2_AUDIENCE=payment-api
EOF

echo "  ✓ .env saved"
echo ""
echo "──────────────────────────────────────────────"
echo " Next:"
echo "   source .env"
echo "   go run ./04_resource_server.go &      # 起 demo API"
echo "   go run ./01_merchant_demo.go          # 商户 SDK 调用"
echo "   go run ./02_service_demo.go           # 服务间调用"
echo "──────────────────────────────────────────────"
