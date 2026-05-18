#!/usr/bin/env bash
# SP-AC-7 PH3-6 chaos #01: accounting gRPC 5s latency
#
# Pre: stack 起来, toxiproxy 在 8474, accounting via toxiproxy 端口 19091.
# 期望: circuit opens after 5 consecutive timeouts, requests fail fast, 60s 内恢复.
set -euo pipefail

SCENARIO="01_accounting_timeout"
FAULT_DESC="accounting gRPC 5s latency"
SLO_RECOVERY_SECONDS=60

TOXI=${TOXIPROXY_URL:-http://localhost:8474}
SP_TRIGGER=${SP_TRIGGER_URL:-http://localhost:19190/api/moneyflow/trigger}
SP_METRICS=${SP_METRICS_URL:-http://localhost:9099/metrics}

log()  { printf '[%s][%s] %s\n' "$(date +%H:%M:%S)" "$SCENARIO" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

# ─── 0. health gate ────────────────────────────────────────────────
log "Pre-flight: toxiproxy reachable?"
curl -sS "$TOXI/proxies" >/dev/null || fail "toxiproxy at $TOXI unreachable"

log "Pre-flight: split-payment metrics reachable?"
curl -sS "$SP_METRICS" >/dev/null || fail "split-payment metrics at $SP_METRICS unreachable"

# ─── 1. ensure toxiproxy proxy "accounting_grpc" exists ────────────
log "Ensuring proxy accounting_grpc exists"
curl -sS -X POST "$TOXI/proxies" -H 'Content-Type: application/json' \
  -d '{"name":"accounting_grpc","listen":"0.0.0.0:19091","upstream":"accounting-system:9091","enabled":true}' \
  >/dev/null 2>&1 || true   # ok if already exists

# ─── 2. inject 5s latency ──────────────────────────────────────────
log "Injecting fault: $FAULT_DESC"
curl -sS -X POST "$TOXI/proxies/accounting_grpc/toxics" \
  -H 'Content-Type: application/json' \
  -d '{"name":"high_latency","type":"latency","stream":"upstream","attributes":{"latency":5000}}' \
  >/dev/null

# ─── 3. drive 10 trigger requests, expect circuit to open ──────────
log "Driving 10 trigger events"
for i in $(seq 1 10); do
  curl -sS -m 8 -X POST "$SP_TRIGGER" \
    -H 'Content-Type: application/json' \
    -d "{\"graph_key\":\"user_topup_e2e\",\"event\":{\"event\":\"charge.succeeded\",\"charge_id\":\"ch_chaos_${i}_$(date +%s)\",\"amount_minor\":1000,\"currency\":\"USD\",\"attributes\":{\"user_id\":\"100000042\"}}}" \
    >/dev/null || true
done

# ─── 4. assert circuit opened (sp_circuit_state{downstream="accounting"} == 1) ──
log "Asserting circuit breaker opened"
sleep 3
STATE=$(curl -sS "$SP_METRICS" | grep -E '^sp_circuit_state\{downstream="accounting"' | awk '{print $2}' | head -1)
if [ "$STATE" != "1" ]; then
  fail "circuit state expected 1 (open), got '$STATE'"
fi
log "Circuit OPEN ✓"

# ─── 5. fast-fail check: requests under open circuit should < 100ms ─
log "Verifying fast-fail latency under open circuit"
T0=$(date +%s%N)
curl -sS -m 5 -X POST "$SP_TRIGGER" \
  -H 'Content-Type: application/json' \
  -d '{"graph_key":"user_topup_e2e","event":{"event":"charge.succeeded","charge_id":"ch_fastfail","amount_minor":1,"currency":"USD"}}' \
  >/dev/null 2>&1 || true
T1=$(date +%s%N)
LAT_MS=$(( (T1 - T0) / 1000000 ))
log "Fast-fail latency: ${LAT_MS}ms"
if [ "$LAT_MS" -gt 1000 ]; then
  fail "fast-fail latency ${LAT_MS}ms > 1000ms, circuit not actually fast-failing"
fi
log "Fast-fail ✓"

# ─── 6. remove fault, wait for recovery ────────────────────────────
log "Removing fault"
curl -sS -X DELETE "$TOXI/proxies/accounting_grpc/toxics/high_latency" >/dev/null

log "Waiting up to ${SLO_RECOVERY_SECONDS}s for circuit to close"
for i in $(seq 1 "$SLO_RECOVERY_SECONDS"); do
  STATE=$(curl -sS "$SP_METRICS" | grep -E '^sp_circuit_state\{downstream="accounting"' | awk '{print $2}' | head -1)
  if [ "$STATE" = "0" ]; then
    log "Circuit CLOSED after ${i}s ✓"
    break
  fi
  sleep 1
done
if [ "$STATE" != "0" ]; then
  fail "circuit did not close within ${SLO_RECOVERY_SECONDS}s (still=$STATE)"
fi

# ─── 7. verify normal trigger succeeds post-recovery ────────────────
log "Verifying normal trigger succeeds post-recovery"
RESP=$(curl -sS -m 5 -X POST "$SP_TRIGGER" \
  -H 'Content-Type: application/json' \
  -d "{\"graph_key\":\"user_topup_e2e\",\"event\":{\"event\":\"charge.succeeded\",\"charge_id\":\"ch_recover_$(date +%s)\",\"amount_minor\":100,\"currency\":\"USD\",\"attributes\":{\"user_id\":\"100000042\"}}}")
if ! echo "$RESP" | grep -q '"plan_id"'; then
  fail "post-recovery trigger failed: $RESP"
fi
log "Post-recovery trigger ✓"

log "Scenario PASSED"
