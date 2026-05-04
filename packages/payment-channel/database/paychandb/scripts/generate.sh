#!/usr/bin/env bash
# 生成 paychan_db_0..9 各自的初始化 SQL（10 库 × 每库 10 张分片表）。
#
# 生成两套：
#   ${OUT}/<db>_init.sql         主表（acquirer_tx / webhook_raw / ...）
#   ${OUT}/<db>_init_shadow.sql  影子表（CREATE TABLE LIKE 主表，专供压测流量）
#
# 部署：
#   生产 / 测试环境只导 *_init.sql
#   压测环境追加 *_init_shadow.sql（依赖主表已建）
#
# payment-channel 启动期还会 ApplyShadowTables 兜底（CREATE … IF NOT EXISTS）；
# 两条路都是 idempotent。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${HERE}/../templates/schema.sql"
OUT="${HERE}/../init"
mkdir -p "${OUT}"

# 影子表的 base name 列表（与 internal/repo/schema_migrator.go 的 shardTableBases
# 完全对齐；新增分片表时两边都要加）
SHADOW_BASES=(
  acquirer_tx
  webhook_raw
  webhook_raw_rejected
  channel_token
)

for db in $(seq 0 9); do
  out="${OUT}/${db}_init.sql"
  out_shadow="${OUT}/${db}_init_shadow.sql"
  : > "${out}"
  : > "${out_shadow}"

  # 主表 init
  echo "CREATE DATABASE IF NOT EXISTS \`paychan_db_${db}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;" >> "${out}"
  echo "USE \`paychan_db_${db}\`;" >> "${out}"
  echo "" >> "${out}"

  # 影子表 init 头注释 + USE
  cat >> "${out_shadow}" <<EOF
-- paychan_db_${db} 的影子表（压测 / shadow 流量）
-- 依赖：${db}_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
USE \`paychan_db_${db}\`;

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
echo "  4 base × 100 = 400 主表 / 400 影子表"
echo "  base 名: ${SHADOW_BASES[*]}"
