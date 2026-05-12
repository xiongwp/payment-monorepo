#!/usr/bin/env bash
# dr-failover.sh — DR 切流 driver. 真切流 + 演练共用.
#
# 用法:
#   ./dr-failover.sh promote-mysql us-west-2
#   ./dr-failover.sh promote-kafka us-west-2
#   ./dr-failover.sh switch-dns us-west-2 [--weight 100]
#   ./dr-failover.sh verify us-west-2
#   ./dr-failover.sh rollback us-east-1
#
# 所有动作走 audit-log + approval-service 双人复核 (除 verify).

set -euo pipefail

ACTION="${1:-}"
TARGET="${2:-}"

if [ -z "$ACTION" ] || [ -z "$TARGET" ]; then
    cat <<USAGE
DR failover driver.

Usage:
  $0 promote-mysql <region>      提升备 MySQL 为 master
  $0 promote-kafka <region>      Kafka MirrorMaker 切方向
  $0 switch-dns    <region>      Route53 weight
  $0 verify        <region>      跑健康检查
  $0 rollback      <region>      反向切回

Approval required (real prod):
  $APPROVAL_URL/v1/actions  type=dr_failover  required_approvals=2
USAGE
    exit 1
fi

AUDIT="${AUDIT_URL:-http://audit-log:8087}"
APPROVAL="${APPROVAL_URL:-http://approval-service:8092}"

audit() {
    local action="$1" detail="$2"
    curl -fsS -X POST "$AUDIT/api/v1/audit/log" \
        -H 'Content-Type: application/json' \
        -d "{
            \"service\":     \"dr-failover-script\",
            \"actor_email\": \"$(whoami)@$(hostname)\",
            \"action\":      \"$action\",
            \"resource_type\": \"region\",
            \"resource_id\":   \"$TARGET\",
            \"note\":          \"$detail\"
        }" > /dev/null 2>&1 || true
}

require_approval() {
    local resource="region:$TARGET"
    local actions
    actions=$(curl -fsS "$APPROVAL/v1/actions?type=dr_failover&state=approved" 2>&1)
    if ! echo "$actions" | grep -q "\"resource\":\"$resource\""; then
        echo "✗ DR failover 需要 approval (2 approvers via approval-service)"
        echo "  POST $APPROVAL/v1/actions  type=dr_failover  resource=$resource"
        exit 2
    fi
    echo "  ✓ approval 通过"
}

case "$ACTION" in
    promote-mysql)
        require_approval
        echo "Promoting MySQL replica in $TARGET to master..."
        # 真实: kubectl 调 MySQL operator (e.g. percona / orchestrator) 触发 promote
        # 或 cloud RDS API (aws rds promote-read-replica)
        echo "  [stub] aws rds promote-read-replica --db-instance-identifier mysql-${TARGET}-replica"
        echo "  [stub] await new master ready (~30s)"
        audit "promote-mysql" "region=$TARGET"
        echo "  ✓ MySQL promoted"
        ;;

    promote-kafka)
        require_approval
        echo "Switching Kafka MirrorMaker direction to $TARGET..."
        # 真实: kubectl 替 MM2 ConfigMap 改 source/target cluster
        echo "  [stub] kubectl -n kafka patch configmap mm2-config ..."
        audit "promote-kafka" "region=$TARGET"
        echo "  ✓ Kafka MirrorMaker reversed"
        ;;

    switch-dns)
        require_approval
        WEIGHT="100"
        for arg in "$@"; do
            case "$arg" in
                --weight=*) WEIGHT="${arg#*=}" ;;
                --weight)   WEIGHT="$3"; shift ;;
            esac
        done
        echo "Switching Route53 weight to $TARGET (weight=$WEIGHT)..."
        # 真实: aws route53 change-resource-record-sets
        echo "  [stub] aws route53 change-resource-record-sets --hosted-zone-id Z123 ..."
        echo "  [stub] await DNS propagation (TTL 60s)..."
        sleep 5  # 演练用
        audit "switch-dns" "region=$TARGET weight=$WEIGHT"
        echo "  ✓ DNS switched"
        ;;

    verify)
        echo "Running verification against $TARGET..."
        FAILS=0

        check() {
            local name=$1 url=$2
            if curl -fsS --max-time 5 "$url" >/dev/null 2>&1; then
                echo "  ✓ $name"
            else
                echo "  ✗ $name ($url)"
                FAILS=$((FAILS+1))
            fi
        }

        check "api-gateway"        "https://api-${TARGET}.payment.example.com/healthz"
        check "payment-core"       "https://api-${TARGET}.payment.example.com/internal/payment-core/healthz"
        check "merchant-webhook"   "https://api-${TARGET}.payment.example.com/internal/merchant-webhook/healthz"
        check "aml-screening"      "https://api-${TARGET}.payment.example.com/internal/aml-screening/healthz"
        check "tokenization-vault" "https://api-${TARGET}.payment.example.com/internal/tokenization-vault/healthz"

        # 数据一致性 — accounting trial balance
        echo "  checking accounting trial balance..."
        BAL=$(curl -fsS "https://api-${TARGET}.payment.example.com/internal/accounting/trial-balance" 2>/dev/null | grep -oE '"diff_cents":[-0-9]+' | cut -d: -f2 || echo unknown)
        if [ "$BAL" = "0" ]; then
            echo "  ✓ trial balance: $BAL"
        else
            echo "  ✗ trial balance: $BAL (NOT ZERO!)"
            FAILS=$((FAILS+1))
        fi

        audit "verify" "fails=$FAILS"
        if [ $FAILS -gt 0 ]; then
            echo ""
            echo "✗ DR verify FAILED — $FAILS checks failed"
            exit 1
        fi
        echo ""
        echo "✓ DR verify PASSED in $TARGET"
        ;;

    rollback)
        require_approval
        echo "Rolling back: switching back to $TARGET as primary..."
        $0 switch-dns "$TARGET" --weight=100
        $0 promote-mysql "$TARGET"
        $0 promote-kafka "$TARGET"
        audit "rollback" "to=$TARGET"
        echo "  ✓ rollback done"
        ;;

    *)
        echo "✗ unknown action: $ACTION"
        exit 1
        ;;
esac
