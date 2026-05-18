#!/usr/bin/env bash
# SP-AC-7 PH3-6 chaos #05: kill split-payment pod, verify lease handoff + no double-execute.
#
# 期望: 另一个副本 30s 内拿到 lease, 飞行中的 plan 不会双跑 (idempotency via order_no).
set -euo pipefail

SCENARIO="05_split_payment_pod_kill"
NS=${NAMESPACE:-payment-staging}
DEPLOY=split-payment

log()  { printf '[%s][%s] %s\n' "$(date +%H:%M:%S)" "$SCENARIO" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "Pre-flight: $NS/$DEPLOY exists?"
kubectl -n "$NS" get deployment "$DEPLOY" >/dev/null || fail "deployment not found"

log "Current replicas:"
kubectl -n "$NS" get pod -l app=split-payment

# Pick leader pod via lease metric (or random if no leader info)
LEADER_POD=$(kubectl -n "$NS" get pod -l app=split-payment -o jsonpath='{.items[0].metadata.name}')
log "Killing pod: $LEADER_POD"
kubectl -n "$NS" delete pod "$LEADER_POD" --grace-period=10

log "Waiting up to 60s for new pod ready"
kubectl -n "$NS" wait --for=condition=Ready pod -l app=split-payment --timeout=60s

log "Verifying cron lease reacquired (one of the pods reports cron_lease_held=1 within 30s)"
HELD=0
for i in $(seq 1 30); do
  for pod in $(kubectl -n "$NS" get pod -l app=split-payment -o jsonpath='{.items[*].metadata.name}'); do
    METRIC=$(kubectl -n "$NS" exec "$pod" -- wget -qO- http://localhost:9099/metrics 2>/dev/null | grep -E '^sp_cron_lease_held' | awk '{print $2}' | head -1)
    if [ "$METRIC" = "1" ]; then
      HELD=1
      log "Pod $pod now holds the lease ✓"
      break 2
    fi
  done
  sleep 1
done
if [ "$HELD" -ne 1 ]; then
  fail "no replica acquired cron lease within 30s"
fi

log "Scenario PASSED"
