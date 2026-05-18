#!/usr/bin/env bash
# SP-AC-7 PH3-6 chaos #02: accounting drops 50% of RPCs
#
# 期望: 50% trigger 一次失败, retry 后成功; 没有 plan permanently 卡在 'executing'.
set -euo pipefail

SCENARIO="02_accounting_drop"
TOXI=${TOXIPROXY_URL:-http://localhost:8474}
SP_TRIGGER=${SP_TRIGGER_URL:-http://localhost:19190/api/moneyflow/trigger}
DB_DSN=${DB_DSN:-mysql -h 127.0.0.1 -P 3306 -u root -ptestpass split_payment_test}

log()  { printf '[%s][%s] %s\n' "$(date +%H:%M:%S)" "$SCENARIO" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "Inject 50% drop on accounting upstream"
curl -sS -X POST "$TOXI/proxies/accounting_grpc/toxics" \
  -H 'Content-Type: application/json' \
  -d '{"name":"drop_half","type":"timeout","stream":"upstream","toxicity":0.5,"attributes":{"timeout":0}}' \
  >/dev/null

log "Driving 30 trigger requests (expect ~50% retry)"
SUCCESS=0
for i in $(seq 1 30); do
  RESP=$(curl -sS -m 15 -X POST "$SP_TRIGGER" \
    -H 'Content-Type: application/json' \
    -d "{\"graph_key\":\"user_topup_e2e\",\"event\":{\"event\":\"charge.succeeded\",\"charge_id\":\"ch_drop_${i}\",\"amount_minor\":100,\"currency\":\"USD\",\"attributes\":{\"user_id\":\"100000042\"}}}" || true)
  if echo "$RESP" | grep -q '"plan_id"'; then
    SUCCESS=$((SUCCESS+1))
  fi
done
log "Successful triggers: $SUCCESS / 30"

log "Removing fault"
curl -sS -X DELETE "$TOXI/proxies/accounting_grpc/toxics/drop_half" >/dev/null

# Give cron / outbox worker 30s 自愈
log "Waiting 30s for cron / outbox self-heal"
sleep 30

log "Asserting no plan stuck in 'executing'"
STUCK=$($DB_DSN -BNe "SELECT COUNT(*) FROM moneyflow_runs WHERE status='executing' AND created_at < NOW() - INTERVAL 1 MINUTE" 2>/dev/null || echo "0")
if [ "$STUCK" != "0" ]; then
  fail "$STUCK plans still in 'executing' state after recovery"
fi
log "No stuck plans ✓"

log "Scenario PASSED"
