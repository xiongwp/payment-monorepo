#!/usr/bin/env bash
# rollback.sh — 一键回滚指定服务到上一稳定版本。
#
# 优先 argo rollouts undo; 兜底 kubectl rollout undo。
# 自动:
#   1. 拿当前版本 hash
#   2. undo 触发
#   3. wait readiness
#   4. 验证 healthz
#   5. 通知 #incidents Slack

set -euo pipefail

SERVICE="${1:-}"
NAMESPACE="${NAMESPACE:-payment}"
SLACK_WEBHOOK="${SLACK_WEBHOOK:-}"
ACTOR="${ACTOR:-$(whoami)}"
REASON="${REASON:-no reason given}"

if [[ -z "$SERVICE" ]]; then
  echo "Usage: $0 <service-name> [REASON=...] [ACTOR=...]"
  echo "Example: REASON='5xx surge after v1.2.3' $0 oauth2-server"
  exit 2
fi

ts() { date -u +'%Y-%m-%dT%H:%M:%SZ'; }
say() { echo "[$(ts)] $1"; }

say "▶ Rollback initiated: $SERVICE (by $ACTOR, reason: $REASON)"

# 1. 拿当前 image (用于事后查"我们回退掉了什么版本")
CURRENT=$(kubectl -n "$NAMESPACE" get rollout "$SERVICE" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null \
  || kubectl -n "$NAMESPACE" get deploy "$SERVICE" -o jsonpath='{.spec.template.spec.containers[0].image}')
say "  current image: $CURRENT"

# 2. undo
if kubectl -n "$NAMESPACE" get rollout "$SERVICE" >/dev/null 2>&1; then
  say "▶ Using argo rollouts undo"
  kubectl argo rollouts undo "$SERVICE" -n "$NAMESPACE"
else
  say "▶ Using kubectl rollout undo (no argo-rollouts CRD found)"
  kubectl -n "$NAMESPACE" rollout undo deployment/"$SERVICE"
fi

# 3. wait
say "▶ Waiting for rollout to settle (max 5min)..."
if kubectl -n "$NAMESPACE" get rollout "$SERVICE" >/dev/null 2>&1; then
  kubectl argo rollouts get rollout "$SERVICE" -n "$NAMESPACE" --watch &
  WATCH_PID=$!
  sleep 1
  kubectl -n "$NAMESPACE" wait --for=condition=Available --timeout=300s rollout/"$SERVICE" 2>/dev/null || true
  kill $WATCH_PID 2>/dev/null || true
else
  kubectl -n "$NAMESPACE" rollout status deployment/"$SERVICE" --timeout=300s
fi

# 4. 验证 healthz
say "▶ Verify post-rollback health"
POD=$(kubectl -n "$NAMESPACE" get pod -l app="$SERVICE" -o jsonpath='{.items[0].metadata.name}')
if kubectl -n "$NAMESPACE" exec "$POD" -- wget -qO- http://localhost:8087/healthz 2>/dev/null | grep -q ok; then
  say "  ✓ healthz OK"
else
  say "  ✗ healthz FAILED — manual intervention required"
fi

# 5. 拿回滚后的 image, 推 metric, 通知 slack
ROLLED_TO=$(kubectl -n "$NAMESPACE" get rollout "$SERVICE" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null \
  || kubectl -n "$NAMESPACE" get deploy "$SERVICE" -o jsonpath='{.spec.template.spec.containers[0].image}')
say "  rolled back: $CURRENT  →  $ROLLED_TO"

if [[ -n "$SLACK_WEBHOOK" ]]; then
  curl -sS -X POST -H 'Content-Type: application/json' \
    -d "{\"text\":\"🔄 *Rollback executed*\n\`service\`: $SERVICE\n\`actor\`: $ACTOR\n\`reason\`: $REASON\n\`from\`: \`$CURRENT\`\n\`to\`: \`$ROLLED_TO\`\"}" \
    "$SLACK_WEBHOOK" > /dev/null
fi

say "✅ Rollback complete"
