#!/usr/bin/env bash
# kek-rotate.sh — KEK 真生产轮换 driver.
#
# 调用方式:
#   ./kek-rotate.sh <env> generate           生成新 KEK
#   ./kek-rotate.sh <env> activate <kek>     加入 active 列表 (双 active 切换期)
#   ./kek-rotate.sh <env> reencrypt <from> <to> [--workers=N]
#   ./kek-rotate.sh <env> verify <kek> [--sample-pct=N]
#   ./kek-rotate.sh <env> set-primary <kek>
#   ./kek-rotate.sh <env> retire <kek>
#   ./kek-rotate.sh <env> check-new-writes [--since=60s]
#
# 全部走 audit-log 留痕; 大动作 (activate / set-primary / retire) 前自动检查 approval-service.

set -euo pipefail

ENV="${1:-}"
ACTION="${2:-}"
ARG3="${3:-}"
ARG4="${4:-}"

if [ -z "$ENV" ] || [ -z "$ACTION" ]; then
  cat <<'USAGE'
KEK rotation driver.

Usage:
  kek-rotate.sh <env> generate
  kek-rotate.sh <env> activate   <new_kek_id>
  kek-rotate.sh <env> reencrypt  <from_kek> <to_kek> [--workers=N]
  kek-rotate.sh <env> verify     <kek_id> [--sample-pct=N]
  kek-rotate.sh <env> set-primary <kek_id>
  kek-rotate.sh <env> retire     <kek_id>
  kek-rotate.sh <env> check-new-writes [--since=DUR]
USAGE
  exit 1
fi

KMS_CLI="${KMS_CLI:-kmsctl --env=$ENV}"
APPROVAL_URL="${APPROVAL_URL:-http://approval-service:8092}"
AUDITLOG_URL="${AUDITLOG_URL:-http://audit-log:8087}"

# require_approval — 检查指定 resource 的 approval action 是否 approved
require_approval() {
  local resource="$1"
  echo "[approval] checking action for resource=$resource ..."
  local actions
  actions=$(curl -fsS "$APPROVAL_URL/v1/actions?type=kms_rotate&state=approved" 2>&1)
  if ! echo "$actions" | grep -q "\"resource\":\"$resource\""; then
    echo "✗ no approved action found for $resource"
    echo "  请先走 approval-service 集齐 3 个 approver (KEK rotation 强制 3-eyes)"
    echo "  POST $APPROVAL_URL/v1/actions"
    exit 2
  fi
  echo "  ✓ approval found"
}

audit() {
  local action="$1"
  local note="$2"
  curl -fsS -X POST -H 'Content-Type: application/json' \
    "$AUDITLOG_URL/api/v1/audit/log" -d "{
      \"service\":      \"kek-rotate-script\",
      \"actor_email\":  \"$(whoami)@$(hostname)\",
      \"action\":       \"$action\",
      \"resource_type\": \"kek\",
      \"resource_id\":   \"$ARG3\",
      \"note\":          \"$note\"
    }" > /dev/null
}

case "$ACTION" in
  generate)
    NEW="kek_$(date +%Y%m%d)_$(openssl rand -hex 4)"
    echo "Generating new KEK: $NEW"
    $KMS_CLI kek create "$NEW" --slot=auto
    KCV=$($KMS_CLI kek kcv "$NEW")
    echo "  ✓ KCV: $KCV"
    audit "generate" "kcv=$KCV"
    echo "$NEW"
    ;;

  activate)
    require_approval "$ARG3"
    echo "Activating $ARG3 (双 active 期开始)..."
    $KMS_CLI kek activate "$ARG3"
    audit "activate" ""
    echo "  ✓ activated"
    ;;

  reencrypt)
    FROM="$ARG3"
    TO="$ARG4"
    WORKERS=$(echo "$@" | grep -oE -- "--workers=[0-9]+" | cut -d= -f2 || echo 50)
    echo "Re-encrypting DEKs from $FROM → $TO with $WORKERS workers..."
    $KMS_CLI dek reencrypt --from="$FROM" --to="$TO" --workers="$WORKERS"
    audit "reencrypt" "from=$FROM to=$TO workers=$WORKERS"
    echo "  ✓ done"
    ;;

  verify)
    PCT=$(echo "$@" | grep -oE -- "--sample-pct=[0-9]+" | cut -d= -f2 || echo 1)
    echo "Verifying $ARG3 with ${PCT}% sample..."
    $KMS_CLI dek verify --kek="$ARG3" --sample-pct="$PCT"
    audit "verify" "sample_pct=$PCT"
    echo "  ✓ all sampled OK"
    ;;

  set-primary)
    require_approval "$ARG3"
    echo "Setting PRIMARY to $ARG3..."
    OLD=$($KMS_CLI kek get-primary)
    $KMS_CLI kek set-primary "$ARG3"
    audit "set-primary" "from=$OLD to=$ARG3"
    echo "  ✓ primary: $OLD → $ARG3"
    ;;

  retire)
    require_approval "$ARG3"
    echo "Retiring $ARG3 (irreversible; HSM 24h purge delay)..."
    read -p "Confirm retiring $ARG3? [yes/no] " yn
    if [ "$yn" != "yes" ]; then
      echo "abort"
      exit 1
    fi
    $KMS_CLI kek retire "$ARG3"
    audit "retire" "permanent"
    echo "  ✓ retired"
    ;;

  check-new-writes)
    SINCE=$(echo "$@" | grep -oE -- "--since=[0-9]+s" | cut -d= -f2 || echo "60s")
    PRIMARY=$($KMS_CLI kek get-primary)
    echo "Checking that all new writes in last $SINCE used kek=$PRIMARY..."
    $KMS_CLI audit check-writes --since="$SINCE" --expected-kek="$PRIMARY"
    echo "  ✓ all new writes used $PRIMARY"
    ;;

  *)
    echo "✗ unknown action: $ACTION"
    exit 1
    ;;
esac
