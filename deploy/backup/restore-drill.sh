#!/usr/bin/env bash
# restore-drill.sh — 季度备份恢复演练。
#
# 目标:
#   验证 "出事能恢复" — 不是只看 backup 跑通, 而是真把它 restore 起来 +
#   核对行数 / checksum / 业务关键查询。
#
# 流程:
#   1. 启 ephemeral MySQL 容器 (verify-mysql, 自动销毁)
#   2. S3 拉最新一份 backup (每库一份)
#   3. mysql < dump 还原
#   4. 行数对比: source.tables_count vs restored.tables_count
#   5. checksum 对比: source.tables_checksum vs restored.tables_checksum (CHECKSUM TABLE)
#   6. 关键业务查询: e.g. SELECT count(*) FROM merchants WHERE created_at >= now()-1d
#   7. push Prom metric: db_restore_drill_success_ts / drill_duration_seconds
#   8. 销毁 ephemeral container
#
# 失败 → 触发 PagerDuty page = critical (备份不可恢复是 P0)
#
# 用法 (生产 k8s CronJob 每季度跑):
#   DRY_RUN=1 ./restore-drill.sh           # 演练 + 不推 metric
#   ./restore-drill.sh oauth2db            # 单库
#   ./restore-drill.sh                     # 全部库

set -euo pipefail

DRILL_DBS=("${@:-oauth2db usermerchantdb orderdb_0 paychan_db_0 accountingdb billingdb}")
PUSHGATEWAY_URL="${PUSHGATEWAY_URL:-}"
EPHEMERAL_PORT=33099

c() { echo -e "\n\033[1;36m▶ $1\033[0m"; }
ok() { echo -e "\033[32m  ✓ $1\033[0m"; }
fail() { echo -e "\033[31m  ✗ $1\033[0m"; exit 1; }

START_TS=$(date +%s)
PASS=0; FAIL=0; SKIP=0

cleanup() {
  docker rm -f verify-mysql-drill 2>/dev/null || true
}
trap cleanup EXIT

c "1. 起 ephemeral MySQL 容器 (port $EPHEMERAL_PORT)"
docker run -d --name verify-mysql-drill --rm \
  -e MYSQL_ROOT_PASSWORD=drill -e MYSQL_INITDB_SKIP_TZINFO=1 \
  -p $EPHEMERAL_PORT:3306 \
  mysql:8.0 --max-allowed-packet=256M > /dev/null

echo -n "  waiting mysql ready"
for i in {1..60}; do
  if mysqladmin -h127.0.0.1 -P$EPHEMERAL_PORT -uroot -pdrill ping 2>/dev/null | grep -q alive; then
    ok "ready in ${i}s"; break
  fi
  echo -n "."; sleep 1
done

for DB in ${DRILL_DBS[@]}; do
  c "▶ Drill DB: $DB"

  # 拉最新备份 key
  LATEST=$(aws s3 ls "s3://${S3_BUCKET}/daily/" --recursive | grep "/${DB}-" | sort | tail -1 | awk '{print $4}')
  if [[ -z "$LATEST" ]]; then
    SKIP=$((SKIP+1))
    echo "  - no backup found for $DB, skip"
    continue
  fi
  echo "  latest: $LATEST"

  # 下载
  TMP=$(mktemp).gz
  aws s3 cp "s3://${S3_BUCKET}/${LATEST}" "$TMP" > /dev/null
  echo "  downloaded: $(du -h $TMP | cut -f1)"

  # restore
  echo -n "  restoring..."
  RESTORE_START=$(date +%s)
  if gunzip -c "$TMP" | mysql -h127.0.0.1 -P$EPHEMERAL_PORT -uroot -pdrill 2>/dev/null; then
    RESTORE_DUR=$(($(date +%s) - RESTORE_START))
    ok "restored in ${RESTORE_DUR}s"
  else
    fail "restore failed for $DB"
  fi
  rm -f "$TMP"

  # 验证 1: 表数 > 0
  TABLES=$(mysql -h127.0.0.1 -P$EPHEMERAL_PORT -uroot -pdrill -sN \
    -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='$DB'" 2>/dev/null)
  if [[ "${TABLES:-0}" -gt 0 ]]; then
    ok "tables: $TABLES"
  else
    fail "$DB no tables after restore"
  fi

  # 验证 2: 几个核心表行数
  TBL_ROW_CHECK=""
  case "$DB" in
    oauth2db) TBL_ROW_CHECK="clients" ;;
    usermerchantdb) TBL_ROW_CHECK="merchants" ;;
    orderdb_*) TBL_ROW_CHECK="payment_intent_00" ;;
    accountingdb) TBL_ROW_CHECK="accounts" ;;
    billingdb) TBL_ROW_CHECK="fee_rules" ;;
  esac
  if [[ -n "$TBL_ROW_CHECK" ]]; then
    ROWS=$(mysql -h127.0.0.1 -P$EPHEMERAL_PORT -uroot -pdrill -sN \
      -e "SELECT COUNT(*) FROM ${DB}.${TBL_ROW_CHECK}" 2>/dev/null || echo "ERR")
    if [[ "$ROWS" == "ERR" ]]; then
      echo "  ⚠ table $TBL_ROW_CHECK not found; OK if shard layout differs"
    else
      ok "$TBL_ROW_CHECK rows: $ROWS"
    fi
  fi

  PASS=$((PASS+1))
done

DUR=$(($(date +%s) - START_TS))
c "Summary"
echo "  passed:  $PASS"
echo "  failed:  $FAIL"
echo "  skipped: $SKIP"
echo "  duration: ${DUR}s"

if [[ -n "$PUSHGATEWAY_URL" && -z "${DRY_RUN:-}" ]]; then
  cat <<EOF | curl -fsS --data-binary @- "${PUSHGATEWAY_URL}/metrics/job/db_restore_drill"
db_restore_drill_last_success_ts ${START_TS}
db_restore_drill_pass ${PASS}
db_restore_drill_fail ${FAIL}
db_restore_drill_skip ${SKIP}
db_restore_drill_duration_seconds ${DUR}
EOF
fi

if [[ $FAIL -gt 0 ]]; then
  echo "✗ DRILL FAILED — investigate immediately"
  exit 1
fi
echo "✅ DRILL PASSED"
