#!/bin/bash
# 生成 card_center_db_0..9 的初始化 SQL（10 库 × 每库 10 张分片表）。
# 跟 user-merchant-core / order-core / accounting-system 同形。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${HERE}/../templates/schema.sql"
OUTPUT_DIR="${HERE}/../init"
mkdir -p "$OUTPUT_DIR"

SHADOW_BASES=(
  card_stored_token
  card_payment_token_used
)

for db in $(seq 0 9); do
  out="${OUTPUT_DIR}/${db}_init.sql"
  out_shadow="${OUTPUT_DIR}/${db}_init_shadow.sql"
  : > "${out}"
  : > "${out_shadow}"
  {
    echo "SET NAMES utf8mb4;"
    echo "CREATE DATABASE IF NOT EXISTS \`card_center_db_${db}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
    echo "USE \`card_center_db_${db}\`;"
    echo ""
  } >> "${out}"
  cat >> "${out_shadow}" <<EOF
-- card_center_db_${db} shadow 表（压测）
USE \`card_center_db_${db}\`;

EOF
  for i in $(seq 0 9); do
    table=$(printf "%02d" $((db * 10 + i)))
    sed -e "s/\${DB}/${db}/g" -e "s/\${TABLE}/${table}/g" "${TEMPLATE}" >> "${out}"
    echo "" >> "${out}"
    for base in "${SHADOW_BASES[@]}"; do
      printf 'CREATE TABLE IF NOT EXISTS `%s_%s_shadow` LIKE `%s_%s`;\n' \
        "${base}" "${table}" "${base}" "${table}" >> "${out_shadow}"
    done
    echo "" >> "${out_shadow}"
  done
done

echo "已生成 ${OUTPUT_DIR}/{0..9}_init.sql + _shadow"
echo "  10 库 × 10 表 × 2 base = 200 张主表 + 200 张 shadow"
