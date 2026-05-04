#!/usr/bin/env bash
# cleanup-shadow.sh — 影子表数据滚动清理（P0-3 资损 / 容量保护）。
#
# 背景：
#   _shadow 表是 CREATE TABLE LIKE 主表，schema 完全相同但**没有 partition / TTL**。
#   生产压测每天可能写入数千万行 shadow 数据；不清理会让主库 IO 被影子表
#   binlog / replication / backup 吃光（备份恢复时间倍增、主从延迟拉高）。
#
# 策略：
#   - 每个 shadow 表按 created_at 删除超过 RETENTION_DAYS 的行
#   - 单批 LIMIT 10000，避免大事务锁表
#   - LOOP 直到当批返回 0 行（删完）
#   - 每个分库串行处理，分库间可以脚本外层 parallelize
#
# 用法：
#   RETENTION_DAYS=30 ./cleanup-shadow.sh                      # 默认全 10 库
#   DB_INDEX=0 RETENTION_DAYS=30 ./cleanup-shadow.sh           # 只跑 db 0
#   DRY_RUN=1 RETENTION_DAYS=30 ./cleanup-shadow.sh            # 只 SELECT count，不 DELETE
#
# 环境变量（必传）：
#   MYSQL_HOST / MYSQL_PORT / MYSQL_USER / MYSQL_PASSWORD
#
# 推荐部署方式（任选其一）：
#   1. K8s CronJob 每天 03:00 UTC 跑一次（流量低谷 / 离日切窗口远）
#   2. crond 容器 + crontab：0 3 * * * /scripts/cleanup-shadow.sh
#   3. 加入 accounting-batchtask 作为新 task type（长期方案，需改 AdminService）
#
# 监控：
#   - 把 stdout pipe 到 metrics（每个表 deleted 行数）
#   - 失败时退出码非 0；告警条件：连续 2 天失败

set -euo pipefail

# ─── 默认配置 ──────────────────────────────────────────────
RETENTION_DAYS="${RETENTION_DAYS:-30}"
BATCH_SIZE="${BATCH_SIZE:-10000}"
DB_INDEX="${DB_INDEX:-}"  # 空 = 全部 0..9
DRY_RUN="${DRY_RUN:-0}"
MAX_BATCHES_PER_TABLE="${MAX_BATCHES_PER_TABLE:-1000}"  # 单表最多 1000×10000 = 1000 万行/次

# ─── MySQL 连接 ────────────────────────────────────────────
MYSQL_HOST="${MYSQL_HOST:?required}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
MYSQL_USER="${MYSQL_USER:?required}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:?required}"

mysql_exec() {
  mysql -h"$MYSQL_HOST" -P"$MYSQL_PORT" -u"$MYSQL_USER" -p"$MYSQL_PASSWORD" \
    --default-character-set=utf8mb4 --batch --raw -N "$@"
}

# 待清理的 shadow 表 base 名（与 templates/schema.sql 里的 SHADOW_BASES 数组对齐）。
SHADOW_BASES=(
  account
  account_transaction
  accounting_voucher
  account_balance_snapshot
  day_cut_control
  async_task
  tcc_transaction
  freeze_compensate_outbox
  distributed_lock
  merchant_info
  transaction_order
  transaction_order_extra
  account_balance_buffer
  tcc_coordinator
  batch_order
  settlement_outbox
)

cleanup_table() {
  local db_name=$1
  local tbl=$2
  local total_deleted=0
  local batch_count=0
  local rows_deleted

  while [ $batch_count -lt $MAX_BATCHES_PER_TABLE ]; do
    if [ "$DRY_RUN" = "1" ]; then
      rows_deleted=$(mysql_exec -e "
        SELECT COUNT(*) FROM \`${db_name}\`.\`${tbl}\`
        WHERE created_at < DATE_SUB(NOW(), INTERVAL ${RETENTION_DAYS} DAY)
        LIMIT ${BATCH_SIZE};
      " 2>&1 || echo 0)
      echo "[DRY] ${db_name}.${tbl}: would-delete=${rows_deleted}"
      break
    fi

    rows_deleted=$(mysql_exec -e "
      DELETE FROM \`${db_name}\`.\`${tbl}\`
      WHERE created_at < DATE_SUB(NOW(), INTERVAL ${RETENTION_DAYS} DAY)
      LIMIT ${BATCH_SIZE};
      SELECT ROW_COUNT();
    " 2>&1 | tail -1)

    if ! [[ "$rows_deleted" =~ ^[0-9]+$ ]]; then
      echo "[ERR] ${db_name}.${tbl}: delete failed -> ${rows_deleted}" >&2
      return 1
    fi

    total_deleted=$((total_deleted + rows_deleted))
    batch_count=$((batch_count + 1))

    if [ "$rows_deleted" -lt "$BATCH_SIZE" ]; then
      break
    fi
    # 每批之间 sleep 100ms，给主从复制 / 业务请求让出 IO
    sleep 0.1
  done

  echo "[OK]  ${db_name}.${tbl}: deleted=${total_deleted} (batches=${batch_count})"
}

cleanup_database() {
  local db_idx=$1
  local db_name="accounting_db_${db_idx}"

  echo "==> ${db_name} (retention=${RETENTION_DAYS}d, batch_size=${BATCH_SIZE})"

  # 该 db 下有 10 张 shard 表（00-09 for db 0, 10-19 for db 1, ...）
  for tbl_idx in $(seq $((db_idx * 10)) $((db_idx * 10 + 9))); do
    tbl_idx_padded=$(printf "%02d" "$tbl_idx")
    for base in "${SHADOW_BASES[@]}"; do
      cleanup_table "$db_name" "${base}_${tbl_idx_padded}_shadow" || true
    done
  done
}

# ─── main ─────────────────────────────────────────────────
START_TS=$(date +%s)

if [ -n "$DB_INDEX" ]; then
  cleanup_database "$DB_INDEX"
else
  for i in 0 1 2 3 4 5 6 7 8 9; do
    cleanup_database "$i"
  done
fi

END_TS=$(date +%s)
echo "==> total elapsed: $((END_TS - START_TS))s"
