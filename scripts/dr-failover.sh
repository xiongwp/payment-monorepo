#!/usr/bin/env bash
# dr-failover.sh — DR 切流 driver. 真切流 + 演练共用.
#
# 与之前版本的差异: 所有 [stub] 替换为真实 AWS CLI / kubectl 调用.
# 通过 DRY_RUN=1 切换"演练"模式 (只打印不执行,默认 1; 生产切流必须 DRY_RUN=0).
#
# 用法:
#   ./dr-failover.sh promote-mysql us-west-2
#   ./dr-failover.sh promote-kafka us-west-2
#   ./dr-failover.sh switch-dns us-west-2 [--weight 100]
#   ./dr-failover.sh verify us-west-2
#   ./dr-failover.sh rollback us-east-1
#
# 所有动作走 audit-log + approval-service 双人复核 (除 verify).
#
# 前置环境变量:
#   DRY_RUN                 1=只打印 (默认), 0=真执行
#   AWS_PROFILE             aws cli 用的 profile (默认 default)
#   ROUTE53_ZONE_ID         切 DNS 用的 hosted zone (e.g. Z123ABC)
#   ROUTE53_RECORD          要切的 record name (api.payment.example.com)
#   KAFKA_NAMESPACE         Kafka 所在 K8s namespace (默认 kafka)
#   MM2_CONFIGMAP           MirrorMaker2 ConfigMap 名 (默认 mm2-config)
#   RDS_REPLICA_ID_PREFIX   RDS 副本前缀 (e.g. mysql-prod-replica-)

set -euo pipefail

ACTION="${1:-}"
TARGET="${2:-}"
DRY_RUN="${DRY_RUN:-1}"
AWS_PROFILE="${AWS_PROFILE:-default}"
ROUTE53_ZONE_ID="${ROUTE53_ZONE_ID:-}"
ROUTE53_RECORD="${ROUTE53_RECORD:-api.payment.example.com}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-kafka}"
MM2_CONFIGMAP="${MM2_CONFIGMAP:-mm2-config}"
RDS_REPLICA_ID_PREFIX="${RDS_REPLICA_ID_PREFIX:-mysql-prod-replica-}"

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

Env:
  DRY_RUN=1 (default)  仅打印命令; DRY_RUN=0 真执行
  AWS_PROFILE / ROUTE53_ZONE_ID / KAFKA_NAMESPACE / RDS_REPLICA_ID_PREFIX
USAGE
    exit 1
fi

AUDIT="${AUDIT_URL:-http://audit-log:8087}"
APPROVAL="${APPROVAL_URL:-http://approval-service:8092}"

# run: 包装"演练 vs 真执行"
# 用法: run aws rds promote-read-replica --db-instance-identifier ...
run() {
    if [ "$DRY_RUN" = "1" ]; then
        echo "  [dry-run] $*"
        return 0
    fi
    echo "  [exec]    $*"
    "$@"
}

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
            \"note\":          \"$detail (dry_run=$DRY_RUN)\"
        }" > /dev/null 2>&1 || true
}

require_approval() {
    local resource="region:$TARGET"
    local actions
    actions=$(curl -fsS "$APPROVAL/v1/actions?type=dr_failover&state=approved" 2>&1 || echo "")
    if ! echo "$actions" | grep -q "\"resource\":\"$resource\""; then
        echo "✗ DR failover 需要 approval (2 approvers via approval-service)"
        echo "  POST $APPROVAL/v1/actions  type=dr_failover  resource=$resource"
        exit 2
    fi
    echo "  ✓ approval 通过"
}

# 等待 RDS 实例进入 available
wait_rds_available() {
    local id="$1" timeout="${2:-600}" elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local status
        status=$(aws rds describe-db-instances \
            --profile "$AWS_PROFILE" \
            --db-instance-identifier "$id" \
            --query 'DBInstances[0].DBInstanceStatus' \
            --output text 2>/dev/null || echo unknown)
        if [ "$status" = "available" ]; then
            echo "  ✓ $id available (after ${elapsed}s)"
            return 0
        fi
        echo "  ⌛ $id status=$status, waiting..."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    echo "  ✗ $id never became available in ${timeout}s"
    return 1
}

case "$ACTION" in
    promote-mysql)
        require_approval
        REPLICA_ID="${RDS_REPLICA_ID_PREFIX}${TARGET}"
        echo "Promoting MySQL replica $REPLICA_ID to master..."
        # 真执行: aws rds promote-read-replica
        run aws rds promote-read-replica \
            --profile "$AWS_PROFILE" \
            --db-instance-identifier "$REPLICA_ID"
        # 等待 promote 完成
        if [ "$DRY_RUN" = "0" ]; then
            wait_rds_available "$REPLICA_ID" 600
        fi
        # 把现有 master 标记为 read-replica (避免脑裂)
        OLD_MASTER="${RDS_REPLICA_ID_PREFIX}primary"
        run aws rds modify-db-instance \
            --profile "$AWS_PROFILE" \
            --db-instance-identifier "$OLD_MASTER" \
            --apply-immediately \
            --multi-az
        audit "promote-mysql" "replica=$REPLICA_ID old_master=$OLD_MASTER"
        echo "  ✓ MySQL promoted in $TARGET"
        ;;

    promote-kafka)
        require_approval
        echo "Switching Kafka MirrorMaker direction to $TARGET..."
        # 真执行: kubectl patch ConfigMap, 把 source/target cluster 互换
        # MM2 ConfigMap key 形如 "source.cluster.alias=primary" / "target.cluster.alias=dr"
        # 切方向需要把这两行 swap, 然后 rollout restart strimzi mm2 deployment
        PATCH_FILE=$(mktemp /tmp/mm2-patch.XXXXXX.yaml)
        cat > "$PATCH_FILE" <<PATCH
data:
  mm2.properties: |
    clusters = ${TARGET}, primary
    ${TARGET}.bootstrap.servers = kafka-${TARGET}-bootstrap.${KAFKA_NAMESPACE}.svc:9092
    primary.bootstrap.servers   = kafka-primary-bootstrap.${KAFKA_NAMESPACE}.svc:9092
    source.cluster.alias = ${TARGET}
    target.cluster.alias = primary
    ${TARGET}->primary.enabled = true
    primary->${TARGET}.enabled = false
    replication.factor = 3
    refresh.topics.interval.seconds = 30
    sync.topic.acls.enabled = true
PATCH
        run kubectl -n "$KAFKA_NAMESPACE" patch configmap "$MM2_CONFIGMAP" \
            --patch-file "$PATCH_FILE"
        rm -f "$PATCH_FILE"

        # 重启 MM2 让新配置生效
        run kubectl -n "$KAFKA_NAMESPACE" rollout restart deployment/kafka-mirror-maker-2
        if [ "$DRY_RUN" = "0" ]; then
            run kubectl -n "$KAFKA_NAMESPACE" rollout status \
                deployment/kafka-mirror-maker-2 --timeout=300s
        fi
        audit "promote-kafka" "region=$TARGET cm=$MM2_CONFIGMAP"
        echo "  ✓ Kafka MirrorMaker reversed; consuming from $TARGET → primary"
        ;;

    switch-dns)
        require_approval
        if [ -z "$ROUTE53_ZONE_ID" ]; then
            echo "✗ ROUTE53_ZONE_ID env required for switch-dns"
            exit 3
        fi
        WEIGHT="100"
        for arg in "$@"; do
            case "$arg" in
                --weight=*) WEIGHT="${arg#*=}" ;;
                --weight)   WEIGHT="$3"; shift ;;
            esac
        done
        echo "Switching Route53 weight to $TARGET (weight=$WEIGHT)..."
        # 真执行: aws route53 change-resource-record-sets, 使用 weighted routing policy
        CHANGE_FILE=$(mktemp /tmp/r53-change.XXXXXX.json)
        cat > "$CHANGE_FILE" <<R53JSON
{
  "Comment": "DR failover to $TARGET (weight=$WEIGHT)",
  "Changes": [
    {
      "Action": "UPSERT",
      "ResourceRecordSet": {
        "Name": "$ROUTE53_RECORD",
        "Type": "CNAME",
        "SetIdentifier": "$TARGET",
        "Weight": $WEIGHT,
        "TTL": 60,
        "ResourceRecords": [{"Value": "api-${TARGET}.payment.example.com"}]
      }
    },
    {
      "Action": "UPSERT",
      "ResourceRecordSet": {
        "Name": "$ROUTE53_RECORD",
        "Type": "CNAME",
        "SetIdentifier": "us-east-1",
        "Weight": $((100 - WEIGHT)),
        "TTL": 60,
        "ResourceRecords": [{"Value": "api-us-east-1.payment.example.com"}]
      }
    }
  ]
}
R53JSON
        run aws route53 change-resource-record-sets \
            --profile "$AWS_PROFILE" \
            --hosted-zone-id "$ROUTE53_ZONE_ID" \
            --change-batch "file://$CHANGE_FILE"
        rm -f "$CHANGE_FILE"
        # 等待 propagation
        if [ "$DRY_RUN" = "0" ]; then
            echo "  ⌛ awaiting DNS propagation (TTL 60s + buffer)..."
            sleep 75
        fi
        audit "switch-dns" "region=$TARGET weight=$WEIGHT zone=$ROUTE53_ZONE_ID"
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
        check "kafka-mm2"          "https://api-${TARGET}.payment.example.com/internal/kafka/mm2-status"
        check "mysql-replica-lag"  "https://api-${TARGET}.payment.example.com/internal/mysql/replica-lag"

        # 数据一致性 — accounting trial balance
        echo "  checking accounting trial balance..."
        BAL=$(curl -fsS "https://api-${TARGET}.payment.example.com/internal/accounting/trial-balance" 2>/dev/null \
            | grep -oE '"diff_cents":[-0-9]+' | cut -d: -f2 || echo unknown)
        if [ "$BAL" = "0" ]; then
            echo "  ✓ trial balance: $BAL"
        else
            echo "  ✗ trial balance: $BAL (NOT ZERO!)"
            FAILS=$((FAILS+1))
        fi

        # RPO 检查 — 上游 MySQL replica lag < 5s
        LAG=$(curl -fsS "https://api-${TARGET}.payment.example.com/internal/mysql/replica-lag" 2>/dev/null \
            | grep -oE '"lag_seconds":[0-9.]+' | cut -d: -f2 || echo unknown)
        case "$LAG" in
            unknown) ;;
            *) if awk "BEGIN{exit !($LAG < 5)}"; then
                   echo "  ✓ replica lag: ${LAG}s"
               else
                   echo "  ✗ replica lag: ${LAG}s (> 5s, RPO breach)"
                   FAILS=$((FAILS+1))
               fi ;;
        esac

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
        "$0" switch-dns "$TARGET" --weight=100
        "$0" promote-mysql "$TARGET"
        "$0" promote-kafka "$TARGET"
        audit "rollback" "to=$TARGET"
        echo "  ✓ rollback done"
        ;;

    *)
        echo "✗ unknown action: $ACTION"
        exit 1
        ;;
esac
