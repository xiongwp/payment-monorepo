#!/bin/bash
# 生成 user_merchant_db_0..9 的初始化 SQL（10 库 × 每库 10 张分片表）。
#
# 输出（写到 ../init/）：
#   ${db}_init.sql            主表 schema (CREATE TABLE)
#   ${db}_init_shadow.sql     影子表 schema (CREATE TABLE LIKE 主表)
#
# 部署：
#   生产 / 测试环境只导 init
#   压测环境追加 init_shadow（依赖主表已建）
set -euo pipefail

# 用脚本自身位置解出绝对路径，cd 在哪都能跑（payment-admin-web 的
# generate-shared-init.sh 会从 monorepo root 调用本脚本）。
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${HERE}/../templates/schema.sql"
OUTPUT_DIR="${HERE}/../init"

mkdir -p "$OUTPUT_DIR"

# 11 张分片业务表 base name；与 templates/schema.sql 里 CREATE TABLE 顺序一致。
SHADOW_BASES=(
  users
  user_profiles
  user_auths
  login_logs
  user_sessions
  user_roles
  user_accounts
  user_settings
  merchants
  merchant_kyc_document
  merchant_channel_secret
)

for db in $(seq 0 9)
do
  FULL_PATH="$OUTPUT_DIR/${db}_init.sql"
  FULL_PATH_SHADOW="$OUTPUT_DIR/${db}_init_shadow.sql"

  # 每次生成前清空，防止重复执行产生重复内容。
  > "$FULL_PATH"
  > "$FULL_PATH_SHADOW"

  # ── 主表 init 头 ───────────────────────────────────────────────────
  {
    echo "SET NAMES utf8mb4;"
    echo "CREATE DATABASE IF NOT EXISTS \`user_merchant_db_${db}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
    echo "USE \`user_merchant_db_${db}\`;"
    echo ""
  } >> "$FULL_PATH"

  # ── 影子表 init 头 ─────────────────────────────────────────────────
  cat >> "$FULL_PATH_SHADOW" <<EOF
-- user_merchant_db_${db} 的影子表（压测 / shadow 流量）
-- 依赖：${db}_init.sql 已经导入完成（CREATE TABLE LIKE 需要主表存在）。
SET NAMES utf8mb4;
USE \`user_merchant_db_${db}\`;

EOF

  # 每库 10 张分片表 (db × 10 + i = globalTblIdx)
  for i in $(seq 0 9)
  do
    table=$(printf "%02d" $((db * 10 + i)))

    # 主表 schema
    sed -e "s/\${DB}/${db}/g" -e "s/\${TABLE}/${table}/g" "$TEMPLATE" >> "$FULL_PATH"
    echo "" >> "$FULL_PATH"

    # 影子表：每个 base 一条 CREATE … LIKE
    for base in "${SHADOW_BASES[@]}"
    do
      printf 'CREATE TABLE IF NOT EXISTS `%s_%s_shadow` LIKE `%s_%s`;\n' \
        "${base}" "${table}" "${base}" "${table}" >> "$FULL_PATH_SHADOW"
    done
    echo "" >> "$FULL_PATH_SHADOW"
  done
done

echo "已生成 init/${db}_init.sql + ${db}_init_shadow.sql (主流量 + 压测影子)"
echo "  10 库 × 每库 10 张 = 100 张全局分片表"
echo "  base 名: ${SHADOW_BASES[*]}"
