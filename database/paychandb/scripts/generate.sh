#!/usr/bin/env bash
# 生成 paychan_db_0..9 各自的初始化 SQL（10 库 × 每库 10 张分片表）
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${HERE}/../templates/schema.sql"
OUT="${HERE}/../init"
mkdir -p "${OUT}"

for db in $(seq 0 9); do
  out="${OUT}/${db}_init.sql"
  : > "${out}"
  echo "CREATE DATABASE IF NOT EXISTS \`paychan_db_${db}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;" >> "${out}"
  echo "USE \`paychan_db_${db}\`;" >> "${out}"
  echo "" >> "${out}"
  for i in $(seq 0 9); do
    global=$((db * 10 + i))
    tbl=$(printf "%02d" "${global}")
    sed "s/\${DB}/${db}/g; s/\${TABLE}/${tbl}/g" "${TEMPLATE}" >> "${out}"
    echo "" >> "${out}"
  done
done
echo "generated ${OUT}/0_init.sql .. 9_init.sql"
