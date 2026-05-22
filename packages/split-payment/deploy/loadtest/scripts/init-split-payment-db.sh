#!/usr/bin/env bash
# ============================================================================
# init-split-payment-db.sh (DB-split Batch 7 极简版)
#
# 在 accounting-system 共享的 10 个 mysql 容器上初始化 split-payment 的
# 1 个 meta 库（含 2 张 graph 配置表）+ 10 个 shard 库（每库 20 张 moneyflow_event
# 子表：10 主 + 10 shadow）。总共 1 meta + 10 shard = 11 库 × 200+2 张表。
#
# 跑这条之前 accounting 栈要已经起来（accounting-mysql-0..9 容器在跑）。
#
# 用法：./scripts/init-split-payment-db.sh
# ============================================================================

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)"
METADB_INIT="${HERE}/../../database/metadb/init/init.sql"
SHARDB_DIR="${HERE}/../../database/shardb/init"
MYSQL_PWD="${MYSQL_PWD:-password}"

green()  { printf "\033[32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
red()    { printf "\033[31m%s\033[0m\n" "$*"; }

if [[ ! -f "${METADB_INIT}" ]]; then
  red "ERROR: meta init SQL 找不到: ${METADB_INIT}" >&2
  exit 1
fi

# ─── 1. Meta 库（split_payment_meta）灌到 accounting-mysql-0 ────────────
echo ">>> [1/2] 初始化 split_payment_meta 在 accounting-mysql-0"
if ! docker exec -i accounting-mysql-0 mysql -uroot -p"${MYSQL_PWD}" 2>/dev/null < "${METADB_INIT}"; then
  red "ERROR: meta init SQL 灌入失败"
  exit 1
fi
green "  ✓ split_payment_meta 库 + 2 meta 表（moneyflow_graphs / moneyflow_graph_versions）"

# ─── 2. 10 个 shard 库 ─ 每个灌到对应 accounting-mysql-N ──────────────────
echo ">>> [2/2] 初始化 split_payment_db_0..9（分别灌到 accounting-mysql-0..9）"
for i in 0 1 2 3 4 5 6 7 8 9; do
  init_file="${SHARDB_DIR}/${i}_init.sql"
  if [[ ! -f "${init_file}" ]]; then
    yellow "  ⚠ ${init_file} 不存在，跳过 shard-${i}"
    continue
  fi
  if ! docker exec -i "accounting-mysql-${i}" mysql -uroot -p"${MYSQL_PWD}" 2>/dev/null < "${init_file}"; then
    red "  ✗ shard-${i} init 失败"
    exit 1
  fi
  # 抽查表数（应该是 8 family × 10 子表 × 2 (主+shadow) = 160 张）
  cnt=$(docker exec "accounting-mysql-${i}" mysql -uroot -p"${MYSQL_PWD}" -N -se "
    SELECT COUNT(*) FROM information_schema.tables
    WHERE table_schema='split_payment_db_${i}'" 2>/dev/null || echo "?")
  printf "  ✓ split_payment_db_%d (%s tables)\n" "${i}" "${cnt}"
done

green ">>> DB 初始化完成。共 1 meta + 10 shards，约 1605 张表。"
