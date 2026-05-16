#!/usr/bin/env bash
# runbook-coverage.sh — 检查 PrometheusRule 中的告警是否都在 RUNBOOK.md 里有 SOP.
#
# 用法: scripts/runbook-coverage.sh <prometheus-rule.yml> <RUNBOOK.md>
#
# 出口码:
#   0 = 100% 覆盖
#   1 = 缺失告警 (打印列表)

set -euo pipefail
RULE_FILE="${1:-deploy/alerts/payment-platform.yml}"
RUNBOOK="${2:-docs/runbooks/RUNBOOK.md}"

if [ ! -f "$RULE_FILE" ] || [ ! -f "$RUNBOOK" ]; then
    echo "usage: $0 <prometheus-rule.yml> <RUNBOOK.md>"
    exit 2
fi

# 提取所有 alert 名 (yq 优先, 否则 grep)
if command -v yq >/dev/null 2>&1; then
    ALERTS=$(yq '.. | select(has("alert")) | .alert' "$RULE_FILE" 2>/dev/null | grep -v '^---$' | sort -u)
else
    ALERTS=$(grep -E '^\s+- alert: ' "$RULE_FILE" | awk '{print $3}' | sort -u)
fi

MISSING=()
for alert in $ALERTS; do
    if ! grep -q "^## $alert" "$RUNBOOK"; then
        MISSING+=("$alert")
    fi
done

TOTAL=$(echo "$ALERTS" | wc -l | tr -d ' ')
COVERED=$((TOTAL - ${#MISSING[@]}))

echo "Runbook coverage: $COVERED / $TOTAL alerts"
if [ ${#MISSING[@]} -gt 0 ]; then
    echo ""
    echo "Missing SOPs in RUNBOOK.md:"
    printf '  - %s\n' "${MISSING[@]}"
    exit 1
fi
echo "✓ Full coverage"
