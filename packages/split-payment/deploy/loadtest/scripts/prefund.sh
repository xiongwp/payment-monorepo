#!/usr/bin/env bash
# ============================================================================
# prefund.sh — 给所有 account 表里 balance=0 的账户充钱
#
# 为啥需要：accounting 的 TCC try 阶段对所有 debit 都做 "available_balance >= amount"
# 校验（不管账户是 Asset/Liability，TCC 的可用余额是统一概念）。新建账户初始余额是 0，
# 跑 loadtest 时所有 debit 都会被 "insufficient available balance" 拒绝。
#
# accounting 没暴露 HTTP adjust 端点，所以最干净的办法是直接 mysql UPDATE。
#
# 实现细节：先用 information_schema 找出每个 shard 的所有 account_NN 表，再用
# 多语句 SQL 一次性发过去（mysql -e 接 stdin 的 SQL stream）。避开了 GROUP_CONCAT
# 默认 1024 字节截断坑（100 张表 × ~150 字节 SQL = 15KB 远超）。
#
# 用法：
#   ./scripts/prefund.sh             # 用默认 mysql 密码 password
#   MYSQL_PWD=foo ./scripts/prefund.sh
# ============================================================================

set -euo pipefail

MYSQL_PWD="${MYSQL_PWD:-password}"
TARGET_BALANCE="${TARGET_BALANCE:-1000000000000000}"  # 1e15 minor units

green()  { printf "\033[32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
red()    { printf "\033[31m%s\033[0m\n" "$*"; }

echo ">>> 给所有 account_NN 表里 balance=0 的账户充 ${TARGET_BALANCE} 余额"

UPDATED_TOTAL_ROWS=0
for i in $(seq 0 9); do
  SHARD=$(docker ps --format '{{.Names}}' | grep -E "accounting.*mysql-${i}\b|mysql-${i}-1" | head -1)
  if [[ -z "${SHARD}" ]]; then
    yellow "  shard-${i}: 容器找不到，跳过"
    continue
  fi

  # 1) 列出所有 account_NN 表（每行一条 "db.table" 字符串）
  TABLES=$(docker exec "${SHARD}" mysql -uroot -p"${MYSQL_PWD}" -N -se "
    SELECT CONCAT(table_schema, '.', table_name)
    FROM information_schema.tables
    WHERE table_schema LIKE 'accounting_db_%'
      AND table_name REGEXP '^account_[0-9]+\$';
  " 2>/dev/null || true)

  if [[ -z "${TABLES}" ]]; then
    yellow "  shard-${i} (${SHARD}): 没找到 account_NN 表"
    continue
  fi

  NUM_TABLES=$(echo "${TABLES}" | wc -l | tr -d ' ')

  # 2) 拼接成多条 UPDATE 一次性发，避免每张表一次 docker exec 的开销
  #    bash 变量没有 1024 字节限制，mysql 客户端也接受任意长度 stdin
  SQL=""
  while IFS= read -r tbl; do
    [[ -z "${tbl}" ]] && continue
    SQL+="UPDATE \`${tbl%%.*}\`.\`${tbl##*.}\` SET balance=${TARGET_BALANCE}, available_balance=${TARGET_BALANCE} WHERE balance=0; "
  done <<< "${TABLES}"

  # 3) 把 SQL 喂进 mysql。-v 打 "Rows matched: N Changed: M Warnings: 0" 一行一表，
  #    grep 出来总 Changed 数。
  CHANGED=$(docker exec -i "${SHARD}" mysql -uroot -p"${MYSQL_PWD}" -v 2>/dev/null <<< "${SQL}" \
            | grep -oE 'Changed: [0-9]+' \
            | awk '{sum+=$2} END {print sum+0}')
  CHANGED=${CHANGED:-0}

  printf "  shard-%d (%s): %s 张 account_NN 表，UPDATE 改了 %s 行\n" "${i}" "${SHARD}" "${NUM_TABLES}" "${CHANGED}"
  UPDATED_TOTAL_ROWS=$((UPDATED_TOTAL_ROWS + CHANGED))
done

green ">>> prefund 完成：共改了 ${UPDATED_TOTAL_ROWS} 行"

# 抽检：第一个 user 账户的余额
POOL_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)/output/account_pool.json"
if [[ -s "${POOL_FILE}" ]]; then
  SAMPLE=$(python3 -c "import json; print(json.load(open('${POOL_FILE}'))['users'][0])" 2>/dev/null || echo "")
  if [[ -n "${SAMPLE}" ]]; then
    echo ">>> 抽检 pool.users[0]=${SAMPLE}"
    FOUND=0
    for i in $(seq 0 9); do
      SHARD=$(docker ps --format '{{.Names}}' | grep -E "accounting.*mysql-${i}\b|mysql-${i}-1" | head -1)
      [[ -z "${SHARD}" ]] && continue
      # 遍历这个 shard 所有 account_NN 表找它
      HIT=$(docker exec "${SHARD}" sh -c "
        for tbl in \$(mysql -uroot -p${MYSQL_PWD} -N -se \"
          SELECT CONCAT(table_schema,'.',table_name) FROM information_schema.tables
          WHERE table_schema LIKE 'accounting_db_%' AND table_name REGEXP '^account_[0-9]+\$'\" 2>/dev/null); do
          mysql -uroot -p${MYSQL_PWD} -N -se \"
            SELECT CONCAT('\$tbl|balance=', balance, '|available=', available_balance)
            FROM \$tbl WHERE account_no='${SAMPLE}' LIMIT 1\" 2>/dev/null
        done
      " 2>/dev/null | head -1 || echo "")
      if [[ -n "${HIT}" ]]; then
        green "  ✓ shard-${i}: ${HIT}"
        FOUND=1
        break
      fi
    done
    if [[ ${FOUND} -ne 1 ]]; then
      yellow "  ⚠ ${SAMPLE} 在 10 shards 都没找到 —— 可能 account_no 编码路由到了未预期 shard"
    fi
  fi
fi
