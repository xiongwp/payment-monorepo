#!/usr/bin/env bash
# secret-rotate.sh — 自动轮换关键密钥 (KMS DEK / mTLS 证书 / DB 密码 / API key).
#
# 用法:
#   ./scripts/secret-rotate.sh kms-dek          # 轮换 KMS data encryption key
#   ./scripts/secret-rotate.sh mtls-cert <svc>  # 轮换某服务 mTLS 证书 (cert-manager renew)
#   ./scripts/secret-rotate.sh db-password <env>  # RDS master password
#   ./scripts/secret-rotate.sh oauth-secret <merchant-id>  # 商户 OAuth client_secret
#
# 所有动作走 audit-log + approval-service 双人复核.
# 通过 DRY_RUN=1 (default) 切到演练模式.

set -euo pipefail

ACTION="${1:-}"
DRY_RUN="${DRY_RUN:-1}"
AWS_PROFILE="${AWS_PROFILE:-default}"

run() {
    if [ "$DRY_RUN" = "1" ]; then echo "  [dry-run] $*"; return 0; fi
    echo "  [exec]    $*"; "$@"
}

audit() {
    local action="$1" detail="$2"
    curl -fsS -X POST "${AUDIT_URL:-http://audit-log:8087}/api/v1/audit/log" \
        -H 'Content-Type: application/json' \
        -d "{\"service\":\"secret-rotate\",\"actor_email\":\"$(whoami)@$(hostname)\",
             \"action\":\"$action\",\"note\":\"$detail (dry_run=$DRY_RUN)\"}" \
        > /dev/null 2>&1 || true
}

require_approval() {
    local res
    res=$(curl -fsS "${APPROVAL_URL:-http://approval-service:8092}/v1/actions?type=secret_rotate&state=approved" 2>&1 || echo "")
    if ! echo "$res" | grep -q "\"resource\":\"$1\""; then
        echo "✗ approval required (2 approvers, resource=$1)"; exit 2
    fi
    echo "  ✓ approval ok"
}

case "$ACTION" in
    kms-dek)
        TARGET="kms:dek"
        require_approval "$TARGET"
        echo "Rotating KMS Data Encryption Key..."
        run kubectl -n payment exec deploy/kms-manage -- /app/kmsctl rotate-dek
        run kubectl -n payment rollout restart deploy/kms-manage
        audit "rotate-kms-dek" "rotated"
        ;;

    mtls-cert)
        SVC="${2:?service name required}"
        TARGET="mtls:$SVC"
        require_approval "$TARGET"
        echo "Rotating mTLS cert for $SVC..."
        # cert-manager 触发 renewal
        run kubectl -n payment annotate certificate "$SVC-cert" \
            cert-manager.io/renew-now="$(date +%s)" --overwrite
        # 等 cert-manager 完成 (60s),滚动重启服务
        if [ "$DRY_RUN" = "0" ]; then sleep 60; fi
        run kubectl -n payment rollout restart deploy/"$SVC"
        audit "rotate-mtls" "service=$SVC"
        ;;

    db-password)
        ENV="${2:?env name required}"
        TARGET="db-password:$ENV"
        require_approval "$TARGET"
        NEW_PW=$(openssl rand -base64 24 | tr -d '/+=')
        echo "Rotating RDS master password for env $ENV..."
        run aws rds modify-db-cluster \
            --profile "$AWS_PROFILE" \
            --db-cluster-identifier "payment-$ENV" \
            --master-user-password "$NEW_PW" \
            --apply-immediately
        # 写到 Secret Manager
        run aws secretsmanager put-secret-value \
            --profile "$AWS_PROFILE" \
            --secret-id "rds/payment-$ENV/master" \
            --secret-string "{\"password\":\"$NEW_PW\"}"
        # 让 external-secrets reconcile (k8s 端 Secret 自动刷新)
        run kubectl -n payment annotate externalsecret payment-rds-credentials \
            force-sync="$(date +%s)" --overwrite
        # 滚动 payment-core / order-core / accounting-system 等 (按需扩展)
        for svc in payment-core order-core accounting-system reconplatform; do
            run kubectl -n payment rollout restart deploy/$svc
        done
        audit "rotate-db-pw" "env=$ENV"
        ;;

    oauth-secret)
        MERCHANT="${2:?merchant_id required}"
        TARGET="oauth:$MERCHANT"
        require_approval "$TARGET"
        echo "Rotating OAuth client_secret for $MERCHANT..."
        run curl -fsS -X POST "http://oauth2-server:8087/admin/clients/$MERCHANT/rotate-secret" \
            -H "Authorization: Bearer ${OAUTH2_ADMIN_TOKEN:-}"
        audit "rotate-oauth" "merchant=$MERCHANT"
        ;;

    *)
        cat <<USAGE
secret-rotate.sh — rotate platform secrets.

Usage:
  $0 kms-dek
  $0 mtls-cert <service>
  $0 db-password <env>
  $0 oauth-secret <merchant_id>

Env:
  DRY_RUN=1 (default)  仅打印
  DRY_RUN=0            真执行
USAGE
        exit 1
        ;;
esac
echo "✓ done (dry_run=$DRY_RUN)"
