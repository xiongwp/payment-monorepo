#!/usr/bin/env bash
# ============================================================================
# verify.sh —— 跑完压测后查 accounting 实际落账多少 transaction_order
#
# 端到端 ground truth：split-payment.TriggerEvent 成功 → accounting CreateTransaction
# → 落到 shared-shard-N.accounting_db_M.transaction_order_NN（100 张全局子表）。
#
# 顺手抽 split-payment 容器日志最常见的 10 个 error，方便定位失败原因。
#
# 用法：MYSQL_PWD=foo ./scripts/verify.sh
# ============================================================================

set -uo pipefail

MYSQL_PWD="${MYSQL_PWD:-password}"

green()  { printf "\033[32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
red()    { printf "\033[31m%s\033[0m\n" "$*"; }

echo ">>> 统计 transaction_order 行数（10 shard × 100 子表）"
TOTAL=0
for i in $(seq 0 9); do
  SHARD="shared-shard-${i}"
  docker ps --format '{{.Names}}' | grep -qx "${SHARD}" || { yellow "  ${SHARD}: 没在跑，跳过"; continue; }

  # 动态拼 UNION ALL —— 比 information_schema 估计准确
  CNT=$(docker exec "${SHARD}" sh -c "
    SQL=\$(mysql -uroot -p${MYSQL_PWD} -N -se \"
      SELECT GROUP_CONCAT(CONCAT('SELECT COUNT(*) FROM \\\`', table_schema, '\\\`.\\\`', table_name, '\\\`') SEPARATOR ' UNION ALL ')
      FROM information_schema.tables
      WHERE table_schema LIKE 'accounting_db_%'
        AND table_name REGEXP '^transaction_order_[0-9]+\$';
    \" 2>/dev/null)
    if [[ -n \"\$SQL\" ]]; then
      echo \"SELECT COALESCE(SUM(c),0) FROM (\$SQL) x(c);\" | mysql -uroot -p${MYSQL_PWD} -N 2>/dev/null
    else
      echo 0
    fi
  " 2>/dev/null | tail -1)
  CNT=${CNT:-0}
  printf "  %s: %s rows (跨所有 transaction_order_NN)\n" "${SHARD}" "${CNT}"
  TOTAL=$((TOTAL + CNT))
done
echo "──────────────────────────────────────────────────────────────────────"
if [[ ${TOTAL} -gt 0 ]]; then
  green "✓ 端到端落账 ${TOTAL} 条 transaction_order"
else
  red "✗ 没有 transaction_order 落库 —— 压测的 booking 全军覆没"
fi

echo
echo ">>> split-payment 错误分布 Top 10"
SP_CTR=$(docker ps --format '{{.Names}}' | grep -E '^stack-split-payment(-[0-9]+)?$|^split-payment(-[0-9]+)?$' | head -1)
if [[ -z "${SP_CTR}" ]]; then
  yellow "  split-payment 容器找不到（按 'stack-split-payment-1' / 'split-payment' 都没匹配）"
else
  docker logs "${SP_CTR}" 2>&1 \
    | grep -oE '"error":"[^"]+"' \
    | sort | uniq -c | sort -rn | head -10 \
    || echo "  (无 error 日志)"
fi
