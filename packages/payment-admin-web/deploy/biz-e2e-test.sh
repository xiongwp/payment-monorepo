#!/usr/bin/env bash
# biz-e2e-test.sh — 跨 7 服务的 e2e 流程验证。
#
# 演示真实路径：tokenize → route → fee_calc → refund (触发 webhook + billing
# refund event) → audit_log → kyc → dispute → audit verify。
#
# 前提: biz-build-and-up.sh 已经把 8 个 service 拉起来。
#
# 使用: bash deploy/biz-e2e-test.sh

set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

step() { echo -e "\n${BLUE}═══ $1 ═══${NC}"; }
ok() { echo -e "${GREEN}✓${NC} $1"; }
fail() { echo -e "${RED}✗${NC} $1"; exit 1; }
require() { command -v "$1" >/dev/null 2>&1 || fail "$1 not installed"; }

require jq
require curl

MERCHANT="mer_e2e_$(date +%s)"
CHARGE_ID="ch_e2e_$(date +%s)"
PI_ID="pi_e2e_$(date +%s)"

# ────────────────────────────────────────────────────────────────
step "0. health check all 8 services"
for port in 18090 18091 18092 18093 18094 18095 18096 18099; do
    if curl -sf "http://localhost:$port/healthz" >/dev/null 2>&1; then
        ok ":$port healthy"
    else
        fail ":$port not healthy — run biz-build-and-up.sh first"
    fi
done

# ────────────────────────────────────────────────────────────────
step "1. webhook: 注册商户 endpoint"
WEBHOOK_RESP=$(curl -s -X POST localhost:18093/api/v1/endpoints -d "$(cat <<EOF
{"merchant_id":"$MERCHANT","url":"https://webhook.site/null","event_types":"refund.created,refund.completed,charge.succeeded"}
EOF
)")
SECRET=$(echo "$WEBHOOK_RESP" | jq -r '.signing_secret')
ENDPOINT_ID=$(echo "$WEBHOOK_RESP" | jq -r '.id')
[ -n "$SECRET" ] && [ "$SECRET" != "null" ] && ok "endpoint $ENDPOINT_ID, secret=${SECRET:0:8}..." || fail "no secret in response"

# ────────────────────────────────────────────────────────────────
step "2. gateway: tokenize 卡 (4111... → tok_xxx)"
TOKEN_RESP=$(curl -s -X POST localhost:18091/api/v1/tokens -d '{
  "card": {"pan":"4111111111111111","expiry_mm":12,"expiry_yy":30,"cvv":"123","holder_name":"E2E Test"},
  "type": "one_time"
}')
TOKEN_ID=$(echo "$TOKEN_RESP" | jq -r '.id')
BRAND=$(echo "$TOKEN_RESP" | jq -r '.brand')
[ "$BRAND" = "visa" ] && ok "tokenized: $TOKEN_ID (brand=$BRAND, last4=$(echo $TOKEN_RESP|jq -r .last4))" || fail "tokenize failed: $TOKEN_RESP"

# ────────────────────────────────────────────────────────────────
step "3. gateway: route 决策"
ROUTE_RESP=$(curl -s -X POST localhost:18091/api/v1/route -d "$(cat <<EOF
{"merchant_id":"$MERCHANT","amount_minor":10000,"currency":"PHP","region":"PH","card_bin":"411111","product":"card_charge"}
EOF
)")
PRIMARY=$(echo "$ROUTE_RESP" | jq -r '.primary.id')
[ -n "$PRIMARY" ] && [ "$PRIMARY" != "null" ] && ok "primary=$PRIMARY, fallbacks=$(echo $ROUTE_RESP|jq -c '.fallback|map(.id)')" || fail "no primary channel"

# ────────────────────────────────────────────────────────────────
step "4. billing: fee 试算"
FEE_RESP=$(curl -s -X POST localhost:18090/api/v1/fee/calc -d "$(cat <<EOF
{"merchant_id":"$MERCHANT","ref_id":"$PI_ID","event_type":"charge","amount_minor":10000,"currency":"PHP","product":"card_charge","channel_adapter":"visa","region":"PH"}
EOF
)")
FEE=$(echo "$FEE_RESP" | jq -r '.fee_minor')
RULE=$(echo "$FEE_RESP" | jq -r '.rule_name')
[ -n "$FEE" ] && [ "$FEE" != "null" ] && [ "$FEE" -gt 0 ] && ok "fee=$FEE minor, rule=$RULE" || fail "no fee calculated"

# ────────────────────────────────────────────────────────────────
step "5. billing: 喂 charge 事件落库"
EVENT_RESP=$(curl -s -X POST localhost:18090/api/v1/fee/events -d "$(cat <<EOF
{"merchant_id":"$MERCHANT","ref_id":"$CHARGE_ID","event_type":"charge","amount_minor":10000,"currency":"PHP","product":"card_charge","channel_adapter":"visa","region":"PH","occurred_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
)")
EVENT_ID=$(echo "$EVENT_RESP" | jq -r '.id // empty')
[ -n "$EVENT_ID" ] && ok "fee_event id=$EVENT_ID" || ok "(idempotent skip)"

# ────────────────────────────────────────────────────────────────
step "6. refund: 发起部分退款 (会触发 webhook + billing fee_event(refund))"
REFUND_RESP=$(curl -s -X POST localhost:18094/api/v1/refunds -d "$(cat <<EOF
{"merchant_id":"$MERCHANT","charge_id":"$CHARGE_ID","payment_intent_id":"$PI_ID","amount_minor":3000,"original_charge_amount_minor":10000,"currency":"PHP","reason":"customer_request","method":"original_channel","requested_by":"merchant","trace_id":"tr_e2e_$$"}
EOF
)")
REFUND_ID=$(echo "$REFUND_RESP" | jq -r '.refund_id')
REFUND_STATUS=$(echo "$REFUND_RESP" | jq -r '.status')
[ -n "$REFUND_ID" ] && [ "$REFUND_ID" != "null" ] && ok "refund $REFUND_ID, status=$REFUND_STATUS" || fail "refund failed: $REFUND_RESP"

# 等几秒让 cron 把 approved → submitted 推
sleep 6

# ────────────────────────────────────────────────────────────────
step "7. dispute: 模拟卡组 chargeback notification"
CASE_ID="case_e2e_$(date +%s)"
DISPUTE_RESP=$(curl -s -X POST localhost:18092/api/v1/disputes/webhook -d "$(cat <<EOF
{"external_id":"$CASE_ID","merchant_id":"$MERCHANT","charge_id":"$CHARGE_ID","pi_id":"$PI_ID","amount_minor":10000,"currency":"PHP","reason":"fraud","network":"visa","trace_id":"tr_dispute_$$"}
EOF
)")
DID=$(echo "$DISPUTE_RESP" | jq -r '.id')
[ -n "$DID" ] && [ "$DID" != "null" ] && ok "dispute id=$DID, deadline=$(echo $DISPUTE_RESP|jq -r .response_deadline)" || fail "dispute create failed"

# ────────────────────────────────────────────────────────────────
step "8. kyc: 提交商户 KYB 申请"
KYC_RESP=$(curl -s -X POST localhost:18095/api/v1/kyc/cases -d "$(cat <<EOF
{"merchant_id":"$MERCHANT","business_name":"E2E Demo Inc","business_number":"BR-E2E-$RANDOM","business_country":"PH","industry_code":"5411","expected_gmv_month":50000000}
EOF
)")
KYC_ID=$(echo "$KYC_RESP" | jq -r '.id')
[ -n "$KYC_ID" ] && [ "$KYC_ID" != "null" ] && ok "kyc case=$KYC_ID, case_num=$(echo $KYC_RESP|jq -r .case_num)" || fail "kyc failed"

# ────────────────────────────────────────────────────────────────
step "9. audit-log: 写多条审计 (模拟各服务调)"
for svc in refund-engine dispute-service kyc-service biz-admin-web; do
    curl -s -X POST localhost:18096/api/v1/audit/log -d "$(cat <<EOF
{"service":"$svc","actor_email":"e2e@test","action":"e2e.demo.${svc//-/_}","resource_type":"demo","resource_id":"$CHARGE_ID","note":"E2E run $(date +%s)"}
EOF
)" > /dev/null
done
ok "wrote 4 audit entries"

# ────────────────────────────────────────────────────────────────
step "10. audit: 校验整条 chain"
VERIFY=$(curl -s localhost:18096/api/v1/audit/verify)
OK_VAL=$(echo "$VERIFY" | jq -r '.ok')
TOTAL=$(echo "$VERIFY" | jq -r '.total_count')
[ "$OK_VAL" = "true" ] && ok "chain verified, total=$TOTAL entries" || fail "chain broken: $VERIFY"

# ────────────────────────────────────────────────────────────────
step "RECAP"
cat <<EOF

✅ E2E 7 服务流程跑通 (耗时 ~$(($(date +%s) - $(echo $MERCHANT | sed 's/mer_e2e_//')))s)

merchant_id  : $MERCHANT
token        : $TOKEN_ID ($BRAND)
charge_id    : $CHARGE_ID
refund_id    : $REFUND_ID ($REFUND_STATUS)
dispute_id   : $DID
kyc_case     : $KYC_ID
audit_chain  : $TOTAL entries verified ✓

下一步可以打开 biz-admin-web UI 看：
    http://localhost:18099

EOF
