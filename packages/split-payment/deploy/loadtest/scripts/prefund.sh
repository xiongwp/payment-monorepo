#!/usr/bin/env bash
# ============================================================================
# prefund.sh — 给所有 account 表里 balance=0 的账户充钱
#
# 为啥需要：accounting 的 TCC try 阶段对所有 debit 都做 "available_balance >= amount"
# 校验（不管账户是 Asset/Liability，TCC 的可用余额是统一概念）。新建账户初始余额是 0，
# 跑 loadtest 时所有 debit 都会被 "insufficient available balance" 拒绝。
#
# accounting 没暴露 HTTP adjust 端点（adjustment_service 只在 internal/service 里），
# 所以最干净的办法是直接 mysql UPDATE。
#
# 用法：
#   ./scripts/prefund.sh             # 用默认 mysql 密码 password
#   MYSQL_PWD=foo ./scripts/prefund.sh
# ============================================================================

set -euo pipefail

MYSQL_PWD="${MYSQL_PWD:-password}"
TARGET_BALANCE="${TARGET_BALANCE:-1000000000000000}"  # 1e15 minor units

# accounting 的 mysql 用 10 shard，每个 shard 容器名 accounting-mysql-N
# 每个容器里有 1 个 db (accountingdb_N) × 100 个 account_NN 子表。
green()  { printf "\033[32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
red()    { printf "\033[31m%s\033[0m\n" "$*"; }

echo ">>> 给所有 account_NN 表里 balance=0 的账户充 ${TARGET_BALANCE} 余额"

UPDATED_TOTAL=0
for i in $(seq 0 9); do
  SHARD=$(docker ps --format '{{.Names}}' | grep -E "accounting.*mysql-${i}\b|mysql-${i}-1" | head -1)
  if [[ -z "${SHARD}" ]]; then
    yellow "  shard-${i}: 容器找不到，跳过"
    continue
  fi

  # 用 information_schema 动态找出所有 account_NN 表（排除 account_transaction /
  # account_log / account_business_type_info 等同前缀的兄弟表）。
  GEN_SQL=$(docker exec "${SHARD}" mysql -uroot -p"${MYSQL_PWD}" -N -se "
    SELECT GROUP_CONCAT(
      CONCAT('UPDATE \`', table_schema, '\`.\`', table_name, '\`',
             ' SET balance=${TARGET_BALANCE}, available_balance=${TARGET_BALANCE}',
             ' WHERE balance=0;')
      SEPARATOR ' '
    )
    FROM information_schema.tables
    WHERE table_schema LIKE 'accountingdb_%'
      AND table_name REGEXP '^account_[0-9]+\$';
  " 2>/dev/null || true)

  if [[ -z "${GEN_SQL}" || "${GEN_SQL}" == "NULL" ]]; then
    yellow "  shard-${i} (${SHARD}): 没找到 account_NN 表"
    continue
  fi

  # 执行批量 UPDATE
  RC=0
  docker exec "${SHARD}" mysql -uroot -p"${MYSQL_PWD}" -e "${GEN_SQL}" 2>/dev/null || RC=$?
  if [[ ${RC} -ne 0 ]]; then
    yellow "  shard-${i} (${SHARD}): UPDATE 部分失败 (rc=${RC})"
  fi

  # 统计 balance > 0 的账户数
  CNT=$(docker exec "${SHARD}" mysql -uroot -p"${MYSQL_PWD}" -N -se "
    SELECT COALESCE(SUM(cnt), 0) FROM (
      SELECT (
        SELECT COUNT(*) FROM information_schema.tables t2
        WHERE t2.table_schema = t.table_schema
          AND t2.table_name = t.table_name
      ) AS cnt
      FROM information_schema.tables t
      WHERE t.table_schema LIKE 'accountingdb_%'
        AND t.table_name REGEXP '^account_[0-9]+\$'
    ) x;
  " 2>/dev/null || echo 0)
  printf "  shard-%d (%s): %s 张 account_NN 表已 UPDATE\n" "${i}" "${SHARD}" "${CNT}"
  UPDATED_TOTAL=$((UPDATED_TOTAL + 1))
done

# 抽检：第一个 user 账户的余额
POOL_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)/output/account_pool.json"
if [[ -s "${POOL_FILE}" ]]; then
  SAMPLE=$(python3 -c "import json; print(json.load(open('${POOL_FILE}'))['users'][0])" 2>/dev/null || echo "")
  if [[ -n "${SAMPLE}" ]]; then
    echo ">>> 抽检 user[0]=${SAMPLE}"
    FOUND=0
    for i in $(seq 0 9); do
      SHARD=$(docker ps --format '{{.Names}}' | grep -E "accounting.*mysql-${i}\b|mysql-${i}-1" | head -1)
      [[ -z "${SHARD}" ]] && continue
      # 在所有 account_NN 表里找
      BAL=$(docker exec "${SHARD}" sh -c "
        for db in \$(mysql -uroot -p${MYSQL_PWD} -N -se 'SHOW DATABASES' 2>/dev/null | grep accountingdb_); do
          mysql -uroot -p${MYSQL_PWD} -N -se \"
            SELECT CONCAT(table_name, '|', balance) FROM \$db.account_00 WHERE account_no='${SAMPLE}' LIMIT 1
            UNION ALL
            SELECT CONCAT(table_name, '|', balance) FROM information_schema.tables t
              JOIN \$db.account_00 a ON a.account_no='${SAMPLE}'
              WHERE t.table_schema='\$db' AND t.table_name REGEXP '^account_[0-9]+\$' LIMIT 1;
          \" 2>/dev/null
        done
      " 2>/dev/null | head -1 || echo "")
      if [[ -n "${BAL}" ]]; then
        green "  ✓ shard-${i}: ${BAL}"
        FOUND=1
        break
      fi
    done
    if [[ ${FOUND} -ne 1 ]]; then
      # 兜底：直接遍历所有可能的子表查
      for i in $(seq 0 9); do
        SHARD=$(docker ps --format '{{.Names}}' | grep -E "accounting.*mysql-${i}\b|mysql-${i}-1" | head -1)
        [[ -z "${SHARD}" ]] && continue
        BAL=$(docker exec "${SHARD}" sh -c "
          for db in \$(mysql -uroot -p${MYSQL_PWD} -N -se 'SHOW DATABASES' 2>/dev/null | grep accountingdb_); do
            for tbl in \$(mysql -uroot -p${MYSQL_PWD} -N -se \"
              SELECT table_name FROM information_schema.tables
              WHERE table_schema='\$db' AND table_name REGEXP '^account_[0-9]+\$'
            \" 2>/dev/null); do
              mysql -uroot -p${MYSQL_PWD} -N -se \"
                SELECT CONCAT('\$db.\$tbl|', balance) FROM \$db.\$tbl WHERE account_no='${SAMPLE}' LIMIT 1;
              \" 2>/dev/null
            done
          done
        " 2>/dev/null | head -1 || echo "")
        if [[ -n "${BAL}" ]]; then
          green "  ✓ shard-${i}: ${BAL}"
          FOUND=1
          break
        fi
      done
    fi
    if [[ ${FOUND} -ne 1 ]]; then
      yellow "  ⚠ pool sample account_no=${SAMPLE} 在所有 shard 都没找到 —— 检查 account_no 编码 / shard 路由"
    fi
  fi
fi

green ">>> prefund 完成 (覆盖了 ${UPDATED_TOTAL} 个 shard)"
