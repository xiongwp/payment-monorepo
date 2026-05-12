#!/usr/bin/env bash
# smoke.sh — tokenization-vault 烟雾测试.
# 用法: bash test/smoke.sh [http://localhost:8089] [admintok]

set -euo pipefail
BASE="${1:-http://localhost:8089}"
TOKEN="${2:-${VAULT_ADMIN_TOKEN:-admintok-dev-CHANGE-IN-PROD}}"
TEST_PAN="${TEST_PAN:-4242424242424242}"

echo "==> 1) /healthz"
curl -fsS "$BASE/healthz"; echo

echo "==> 2) Exchange PAN → internal_token (Visa)"
EX=$(curl -fsS -X POST "$BASE/v1/tokens/exchange" -H 'Content-Type: application/json' -d "{
  \"pan\":         \"$TEST_PAN\",
  \"exp_month\":   12,
  \"exp_year\":    2028,
  \"cardholder\":  \"John Doe\",
  \"merchant_id\": \"m_smoke\"
}")
echo "$EX"
INTERNAL_TOKEN=$(echo "$EX" | sed -E 's/.*"token":"([^"]+)".*/\1/')
echo "→ internal_token = $INTERNAL_TOKEN"
echo

echo "==> 3) Dedup: 同商户同卡再调一次应返回同 token"
DEDUP=$(curl -fsS -X POST "$BASE/v1/tokens/exchange" -H 'Content-Type: application/json' -d "{
  \"pan\": \"$TEST_PAN\", \"exp_month\": 12, \"exp_year\": 2028, \"merchant_id\": \"m_smoke\"
}")
DEDUP_TOKEN=$(echo "$DEDUP" | sed -E 's/.*"token":"([^"]+)".*/\1/')
if [[ "$DEDUP_TOKEN" != "$INTERNAL_TOKEN" ]]; then
  echo "DEDUP FAIL: $DEDUP_TOKEN != $INTERNAL_TOKEN"
  exit 1
fi
echo "✓ dedup ok"
echo

echo "==> 4) Provision (sync trigger)"
curl -fsS -X POST "$BASE/v1/tokens/$INTERNAL_TOKEN/provision"
echo

echo "==> 5) ChargeIntent CIT (用户在场, 一次性扣款)"
curl -fsS -X POST "$BASE/v1/tokens/$INTERNAL_TOKEN/charge" -H 'Content-Type: application/json' -d "{
  \"merchant_id\": \"m_smoke\",
  \"amount\": 1999,
  \"currency\": \"USD\",
  \"intent_id\": \"intent_smoke_$RANDOM\",
  \"recurring\": false
}"
echo

echo "==> 6) ChargeIntent MIT (recurring subscription)"
curl -fsS -X POST "$BASE/v1/tokens/$INTERNAL_TOKEN/charge" -H 'Content-Type: application/json' -d "{
  \"merchant_id\": \"m_smoke\",
  \"amount\": 999,
  \"currency\": \"USD\",
  \"intent_id\": \"sub_smoke_$RANDOM\",
  \"recurring\": true
}"
echo

echo "==> 7) 拿 meta (不返 PAN)"
curl -fsS "$BASE/v1/tokens/$INTERNAL_TOKEN"
echo

echo "==> 8) Suspend"
curl -fsS -X POST "$BASE/v1/tokens/$INTERNAL_TOKEN/suspend" -H 'Content-Type: application/json' -d '{"reason":"smoke_test"}'
echo

echo "==> 9) 被冻结后 charge 应失败"
if curl -fsS -X POST "$BASE/v1/tokens/$INTERNAL_TOKEN/charge" -H 'Content-Type: application/json' -d "{
  \"amount\": 100, \"currency\": \"USD\"
}" 2>/dev/null; then
  echo "FAIL: 冻结后 charge 应该 400"
  exit 1
fi
echo "✓ suspended token rejects charge"

echo "==> 10) Admin: 商户 active token 数 (应为 0, 因为刚被 suspend)"
curl -fsS -H "X-Admin-Token: $TOKEN" "$BASE/admin/metrics/merchants/m_smoke/count"
echo

echo "✓ vault smoke ok"
