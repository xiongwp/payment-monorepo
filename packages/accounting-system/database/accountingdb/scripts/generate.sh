#!/bin/bash

TEMPLATE=../templates/schema.sql
TEMPLATE_INIT=../templates/init_tmp.sql
OUTPUT_DIR=../init/

mkdir -p "$OUTPUT_DIR"

for db in $(seq 0 9)
do
  FULL_PATH="$OUTPUT_DIR/${db}_init.sql"
  FULL_PATH_INIT="$OUTPUT_DIR/${db}_init_tmp.sql"

  # 每次生成前清空文件，防止重复运行产生重复内容
  > "$FULL_PATH"
  > "$FULL_PATH_INIT"

  # 每个库只写一次 CREATE DATABASE / USE。
  # 同时强制 session 走 utf8mb4，防止 server 默认 latin1 把中文 comment / INSERT
  # 按 latin1 存成 "ç"¨æˆ·" 这类 mojibake。
  {
    echo "SET NAMES utf8mb4;"
    echo "SET CHARACTER SET utf8mb4;"
    echo "CREATE DATABASE IF NOT EXISTS \`accounting_db_${db}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
    echo "USE \`accounting_db_${db}\`;"
    echo ""
  } >> "$FULL_PATH"

  {
    echo "SET NAMES utf8mb4;"
    echo "SET CHARACTER SET utf8mb4;"
    echo "USE \`accounting_db_${db}\`;"
    echo ""
  } >> "$FULL_PATH_INIT"

  for i in $(seq 0 9)
  do
    table=$(printf "%02d" $((db * 10 + i)))

    sed "s/\${DB}/${db}/g; s/\${TABLE}/${table}/g" "$TEMPLATE" >> "$FULL_PATH"
    echo "" >> "$FULL_PATH"

    sed "s/\${DB}/${db}/g; s/\${TABLE}/${table}/g" "$TEMPLATE_INIT" >> "$FULL_PATH_INIT"
    echo "" >> "$FULL_PATH_INIT"
  done
done

echo "已生成 init/*.sql（10库100表，每库10表，共140张表×10库）"