#!/usr/bin/env bash
# 生成 order_db_0..9 各自的初始化 SQL（10 库 × 每库 10 张分片表）。
#
# 生成两套：
#   ${OUT}/<db>_init.sql         主表（payment_intent / charge / refund / ...）
#   ${OUT}/<db>_init_shadow.sql  影子表（CREATE TABLE LIKE 主表，专供压测流量）
#
# 部署：
#   生产 / 测试环境只需要导入 *_init.sql
#   压测环境追加导入 *_init_shadow.sql（依赖主表已建）
#
# order-core 启动期还会 ApplyShadowTables 兜底（CREATE … IF NOT EXISTS）；
# init_shadow.sql 早跑过 → 兜底是 no-op。两条路都 idempotent。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${HERE}/../templates/schema.sql"
OUT="${HERE}/../init"
mkdir -p "${OUT}"

# 影子表的 base name 列表（必须与 internal/repo/schema_migrator.go 的
# shardTableBases 完全对齐；新增分片表时两边都要加）
SHADOW_BASES=(
  payment_intent
  charge
  refund
  pay_action
  dispute
  dispute_event
  inbound_webhook
  notify_log
  accounting_outbox
)

for db in $(seq 0 9); do
  out="${OUT}/${db}_init.sql"
  out_shadow="${OUT}/${db}_init_shadow.sql"
  : > "${out}"
  : > "${out_shadow}"

  # 主表 init
  echo "CREATE DATABASE IF NOT EXISTS \`order_db_${db}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;" >> "${out}"
  echo "USE \`order_db_${db}\`;" >> "${out}"
  echo "" >> "${out}"

  # 影子表 init 头注释 + USE
  cat >> "${out_shadow}" <<EOF
-- order_db_${db} 的影子表（压测 / shadow 流量）
-- 依赖：${db}_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE \`order_db_${db}\`;

EOF

  for i in $(seq 0 9); do
    global=$((db * 10 + i))
    tbl=$(printf "%02d" "${global}")
    # 主表
    sed "s/\${DB}/${db}/g; s/\${TABLE}/${tbl}/g" "${TEMPLATE}" >> "${out}"
    echo "" >> "${out}"
    # 影子表：每个 base 一条 CREATE … LIKE
    for base in "${SHADOW_BASES[@]}"; do
      printf 'CREATE TABLE IF NOT EXISTS `%s_%s_shadow` LIKE `%s_%s`;\n' \
        "${base}" "${tbl}" "${base}" "${tbl}" >> "${out_shadow}"
    done
    echo "" >> "${out_shadow}"
  done
done
echo "generated ${OUT}/{0..9}_init.sql + {0..9}_init_shadow.sql"
