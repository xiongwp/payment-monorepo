#!/bin/bash
# 生成 accounting_db_0..9 的初始化 SQL（10 库 × 每库 10 张分片表）。
#
# 四套输出：
#   ${db}_init.sql            — 主表 schema (CREATE TABLE)
#   ${db}_init_tmp.sql        — 主表 fleet seed/INSERT
#   ${db}_init_shadow.sql     — 影子表 schema (CREATE TABLE LIKE 主表)
#   ${db}_init_tmp_shadow.sql — 影子 fleet seed/INSERT（user_id 用 shadow 段偏移）
#
# fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
#   主流量：MainFleetUserIDMin   = 1_000_000        + globalTableIdx
#   shadow：ShadowFleetUserIDMin = 9_000_000_000    + globalTableIdx
#
# 部署：
#   生产 / 测试环境只导 init + init_tmp
#   压测环境追加 init_shadow + init_tmp_shadow（依赖主表已建）

TEMPLATE=../templates/schema.sql
TEMPLATE_INIT=../templates/init_tmp.sql
OUTPUT_DIR=../init/

# fleet user_id 段起点（与 identity.go 同步）
MAIN_FLEET_OFFSET=1000000
SHADOW_FLEET_OFFSET=9000000000

mkdir -p "$OUTPUT_DIR"

# 16 张分片业务表 base name；与 templates/schema.sql 里 CREATE TABLE 顺序一致。
# 新增表时这里同步登记，否则影子表不会生成。
SHADOW_BASES=(
  account
  account_transaction
  accounting_voucher
  account_balance_snapshot
  day_cut_control
  async_task
  tcc_transaction
  freeze_compensate_outbox
  distributed_lock
  merchant_info
  transaction_order
  transaction_order_extra
  account_balance_buffer
  tcc_coordinator
  batch_order
  settlement_outbox
)

for db in $(seq 0 9)
do
  FULL_PATH="$OUTPUT_DIR/${db}_init.sql"
  FULL_PATH_INIT="$OUTPUT_DIR/${db}_init_tmp.sql"
  FULL_PATH_SHADOW="$OUTPUT_DIR/${db}_init_shadow.sql"
  FULL_PATH_INIT_SHADOW="$OUTPUT_DIR/${db}_init_tmp_shadow.sql"

  # 每次生成前清空文件，防止重复运行产生重复内容
  > "$FULL_PATH"
  > "$FULL_PATH_INIT"
  > "$FULL_PATH_SHADOW"
  > "$FULL_PATH_INIT_SHADOW"

  # ── ${db}_init.sql 头 ───────────────────────────────────────────────
  {
    echo "SET NAMES utf8mb4;"
    echo "SET CHARACTER SET utf8mb4;"
    echo "CREATE DATABASE IF NOT EXISTS \`accounting_db_${db}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
    echo "USE \`accounting_db_${db}\`;"
    echo ""
  } >> "$FULL_PATH"

  # ── ${db}_init_tmp.sql 头 ──────────────────────────────────────────
  {
    echo "SET NAMES utf8mb4;"
    echo "SET CHARACTER SET utf8mb4;"
    echo "USE \`accounting_db_${db}\`;"
    echo ""
  } >> "$FULL_PATH_INIT"

  # ── ${db}_init_shadow.sql 头 ───────────────────────────────────────
  cat >> "$FULL_PATH_SHADOW" <<EOF
-- accounting_db_${db} 的影子表（压测 / shadow 流量）
-- 依赖：${db}_init.sql 必须已经导入完成（CREATE TABLE LIKE 需要主表存在）
SET NAMES utf8mb4;
USE \`accounting_db_${db}\`;

EOF

  # ── ${db}_init_tmp_shadow.sql 头 ───────────────────────────────────
  cat >> "$FULL_PATH_INIT_SHADOW" <<EOF
-- accounting_db_${db} 的影子 fleet 平台账户 seed
-- 依赖：${db}_init_shadow.sql 必须已经导入完成（_shadow 表已建）
-- fleet user_id 段：[${SHADOW_FLEET_OFFSET}, ${SHADOW_FLEET_OFFSET}+99]
SET NAMES utf8mb4;
USE \`accounting_db_${db}\`;

EOF

  for i in $(seq 0 9)
  do
    table=$(printf "%02d" $((db * 10 + i)))

    # 主表 schema
    sed "s/\${DB}/${db}/g; s/\${TABLE}/${table}/g" "$TEMPLATE" >> "$FULL_PATH"
    echo "" >> "$FULL_PATH"

    # 主流量 fleet seed: USER_ID_OFFSET=MAIN_FLEET_OFFSET=1_000_000, SHADOW_FLAG=0
    sed -e "s/\${DB}/${db}/g" \
        -e "s/\${TABLE}/${table}/g" \
        -e "s/\${USER_ID_OFFSET}/${MAIN_FLEET_OFFSET}/g" \
        -e "s/\${SHADOW_FLAG}/0/g" \
        "$TEMPLATE_INIT" >> "$FULL_PATH_INIT"
    echo "" >> "$FULL_PATH_INIT"

    # 影子表 schema：每个 base 一条 CREATE … LIKE
    for base in "${SHADOW_BASES[@]}"
    do
      printf 'CREATE TABLE IF NOT EXISTS `%s_%s_shadow` LIKE `%s_%s`;\n' \
        "${base}" "${table}" "${base}" "${table}" >> "$FULL_PATH_SHADOW"
    done
    echo "" >> "$FULL_PATH_SHADOW"

    # 影子 fleet seed: USER_ID_OFFSET=SHADOW_FLEET_OFFSET=9_000_000_000, SHADOW_FLAG=1
    # 表名同时改成 *_shadow 后缀
    sed -e "s/\${DB}/${db}/g" \
        -e "s/\${TABLE}/${table}/g" \
        -e "s/\${USER_ID_OFFSET}/${SHADOW_FLEET_OFFSET}/g" \
        -e "s/\${SHADOW_FLAG}/1/g" \
        -e "s/\`account_${table}\`/\`account_${table}_shadow\`/g" \
        "$TEMPLATE_INIT" >> "$FULL_PATH_INIT_SHADOW"
    echo "" >> "$FULL_PATH_INIT_SHADOW"
  done
done

echo "已生成 init/*.sql + init_tmp.sql + init_shadow.sql + init_tmp_shadow.sql"
echo "  主流量 fleet user_id  段: [${MAIN_FLEET_OFFSET}, $((MAIN_FLEET_OFFSET + 99))]（每分片）"
echo "  shadow fleet user_id  段: [${SHADOW_FLEET_OFFSET}, $((SHADOW_FLEET_OFFSET + 99))]（每分片）"
