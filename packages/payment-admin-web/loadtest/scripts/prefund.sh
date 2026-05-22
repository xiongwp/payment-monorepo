#!/usr/bin/env bash
# ============================================================================
# prefund.sh —— 把所有 accounting_db_N.account_NN 表里 balance=0 的账户充满
#
# 为啥需要：accounting TCC try 阶段对所有 debit 都做 "available_balance >= amount"
# 校验。bootstrap 新建的账户初始余额是 0，loadtest 第一笔 debit 一定报
# insufficient available balance。先充 1e15 minor 进去 = 1e13 PHP，够整轮压测花。
#
# 适配新栈：mysql 不再是 accounting-mysql-N，是 payment-admin-web stack 的
# shared-shard-N（DB-split Batch 6 切过来的）。
#
# 幂等：UPDATE ... WHERE balance=0 只动初始账户，重跑不影响有交易的账户。
#
# 用法：
#   ./scripts/prefund.sh
#   MYSQL_PWD=foo ./scripts/prefund.sh
# ============================================================================

set -uo pipefail
# 不用 set -e：mysql 输出格式不稳，pipefail 容易把成功 update 误判失败。
# 每条命令显式 `|| true` 兜底。

MYSQL_PWD="${MYSQL_PWD:-password}"
TARGET_BALANCE="${TARGET_BALANCE:-1000000000000000}"  # 1e15 minor units

green()  { printf "\033[32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
red()    { printf "\033[31m%s\033[0m\n" "$*"; }

echo ">>> prefund: 给 accounting_db_*.account_* 里 balance=0 的账户充 ${TARGET_BALANCE} minor"

UPDATED_TOTAL_ROWS=0
for i in $(seq 0 9); do
  # payment-admin-web stack 用 shared-shard-N 命名
  SHARD="shared-shard-${i}"
  if ! docker ps --format '{{.Names}}' | grep -qx "${SHARD}"; then
    yellow "  shard-${i}: 容器 ${SHARD} 没在跑，跳过"
    continue
  fi

  TABLES=$(docker exec "${SHARD}" mysql -uroot -p"${MYSQL_PWD}" -N -se "
    SELECT CONCAT(table_schema, '.', table_name)
    FROM information_schema.tables
    WHERE table_schema LIKE 'accounting_db_%'
      AND table_name REGEXP '^account_[0-9]+\$';
  " 2>/dev/null || true)

  if [[ -z "${TABLES}" ]]; then
    yellow "  ${SHARD}: 没找到 account_NN 表（accounting 是不是还没起？）"
    continue
  fi

  NUM_TABLES=$(echo "${TABLES}" | wc -l | tr -d ' ')

  # 拼一发 multi-statement SQL 一次性灌进去
  SQL=""
  while IFS= read -r tbl; do
    [[ -z "${tbl}" ]] && continue
    SQL+="UPDATE \`${tbl%%.*}\`.\`${tbl##*.}\` SET balance=${TARGET_BALANCE}, available_balance=${TARGET_BALANCE} WHERE balance=0; "
  done <<< "${TABLES}"

  docker exec -i "${SHARD}" mysql -uroot -p"${MYSQL_PWD}" 2>/dev/null <<< "${SQL}" || true

  # 跑完拿 balance > 0 的真实账户数
  CHANGED=$(docker exec "${SHARD}" sh -c "
    SQL=\$(mysql -uroot -p${MYSQL_PWD} -N -se \"
      SELECT GROUP_CONCAT(CONCAT('SELECT COUNT(*) FROM \\\`', table_schema, '\\\`.\\\`', table_name, '\\\`', ' WHERE balance>0') SEPARATOR ' UNION ALL ')
      FROM information_schema.tables
      WHERE table_schema LIKE 'accounting_db_%' AND table_name REGEXP '^account_[0-9]+\$';
    \" 2>/dev/null)
    [[ -n \"\$SQL\" ]] && echo \"SELECT SUM(c) FROM (\$SQL) x(c);\" | mysql -uroot -p${MYSQL_PWD} -N 2>/dev/null
  " 2>/dev/null || echo "0")
  CHANGED=${CHANGED:-0}

  printf "  %s: %s 张 account_NN 表 → balance>0 的账户 %s 行\n" "${SHARD}" "${NUM_TABLES}" "${CHANGED}"
  UPDATED_TOTAL_ROWS=$((UPDATED_TOTAL_ROWS + CHANGED))
done

green ">>> prefund 完成：累计 balance>0 账户 ${UPDATED_TOTAL_ROWS} 行"

# 抽检：pool 里第一个 user 账户余额
POOL_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)/output/account_pool.json"
if [[ -s "${POOL_FILE}" ]]; then
  SAMPLE=$(python3 -c "import json; print(json.load(open('${POOL_FILE}'))['users'][0])" 2>/dev/null || echo "")
  if [[ -n "${SAMPLE}" ]]; then
    echo ">>> 抽检 pool.users[0]=${SAMPLE}"
    FOUND=0
    for i in $(seq 0 9); do
      SHARD="shared-shard-${i}"
      docker ps --format '{{.Names}}' | grep -qx "${SHARD}" || continue
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
        green "  ✓ ${SHARD}: ${HIT}"
        FOUND=1
        break
      fi
    done
    [[ ${FOUND} -ne 1 ]] && yellow "  ⚠ ${SAMPLE} 在 10 个 shard 都没找到（account_no 编码路由可能不是 0..9 简单 mod？）"
  fi
fi
