#!/usr/bin/env bash
# smoke.sh — aml-screening 烟雾测试.
# 用法: bash test/smoke.sh [http://localhost:8088] [admintok]

set -euo pipefail
BASE="${1:-http://localhost:8088}"
TOKEN="${2:-${AML_ADMIN_TOKEN:-admintok-dev-CHANGE-IN-PROD}}"

echo "==> 1) /healthz"
curl -fsS "$BASE/healthz" | head -c 200
echo ""

echo "==> 2) screen 无命中 (random name)"
curl -fsS -X POST "$BASE/v1/screen" -H 'Content-Type: application/json' -d '{
  "request_id": "smoke-pass-1",
  "trigger":    "kyb_onboarding",
  "subject":    "individual",
  "name":       "Alice Random Citizen",
  "merchant_id": "m_smoke_1"
}' | head -c 500
echo ""

echo "==> 3) screen 命中 OFAC seed (John Doe + DOB + nat)"
curl -fsS -X POST "$BASE/v1/screen" -H 'Content-Type: application/json' -d '{
  "request_id": "smoke-hit-1",
  "trigger":    "kyb_onboarding",
  "subject":    "individual",
  "name":       "John Doe",
  "dob":        "1970-01-01",
  "nationality": "Iran",
  "merchant_id": "m_smoke_2"
}' | head -c 1500
echo ""

echo "==> 4) screen fuzzy name (Jon Doe)"
curl -fsS -X POST "$BASE/v1/screen" -H 'Content-Type: application/json' -d '{
  "request_id": "smoke-fuzzy-1",
  "trigger":    "payout",
  "name":       "Jon Doe",
  "amount":     500000,
  "currency":   "USD"
}' | head -c 1000
echo ""

echo "==> 5) admin: list entries count"
curl -fsS -H "X-Admin-Token: $TOKEN" "$BASE/admin/lists/ofac_sdn/count"
echo ""

echo "==> 6) admin: pending hits"
curl -fsS -H "X-Admin-Token: $TOKEN" "$BASE/admin/hits/pending?limit=5" | head -c 800
echo ""

echo "✓ smoke ok"
