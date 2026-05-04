#!/usr/bin/env bash
# 把 payment-channel 和 order-core 的 per-shard init SQL 拼在一起，
# 产出给「共享 MySQL 分库」用的 10 份 init 文件 + 1 份 meta init。
#
# 产物放到 deploy/shared-db/init/ 下。stack.yml mount 这里进每个 shard 容器。
#
# 约定的目录结构（五仓同级）：
#   <root>/
#     ├── order-core/
#     ├── payment-channel/
#     └── payment-admin-web/
#         └── deploy/shared-db/
#             ├── docker-compose.yml           ← 11 个 MySQL 容器
#             ├── generate-shared-init.sh      ← 本脚本
#             └── init/                        ← 生成的 SQL（gitignored）
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
OUT="$HERE/init"
mkdir -p "$OUT"

PAYCHAN_GEN="$ROOT/payment-channel/database/paychandb/scripts/generate.sh"
ORDER_GEN="$ROOT/order-core/database/orderdb/scripts/generate.sh"
ACCT_INIT_DIR="$ROOT/accounting-system/database/accountingdb/init"
ACCT_META_INIT="$ROOT/accounting-system/database/metadb/init/init.sql"

[[ -x "$PAYCHAN_GEN" ]] || { echo "FATAL: $PAYCHAN_GEN 不存在或不可执行"; exit 1; }
[[ -x "$ORDER_GEN"   ]] || { echo "FATAL: $ORDER_GEN 不存在或不可执行"; exit 1; }
[[ -d "$ACCT_INIT_DIR" ]] || { echo "FATAL: $ACCT_INIT_DIR 不存在（accounting-system 仓需平级）"; exit 1; }
[[ -s "$ACCT_META_INIT" ]] || { echo "FATAL: $ACCT_META_INIT 不存在"; exit 1; }

# ① 确保两仓各自的 per-shard init SQL 已生成
(cd "$ROOT/payment-channel" && rm -rf database/paychandb/init && bash database/paychandb/scripts/generate.sh) >/dev/null
# order-core 的 init 已在 git 里；只在没有/是目录时重拉一次
for i in 0 1 2 3 4 5 6 7 8 9; do
  f="$ROOT/order-core/database/orderdb/init/${i}_init.sql"
  if [[ -d "$f" || ! -s "$f" ]]; then
    (cd "$ROOT/order-core" && rm -rf database/orderdb/init && git checkout database/orderdb/init) 2>/dev/null || true
  fi
  [[ -s "$f" ]] || { echo "FATAL: order-core 缺 ${i}_init.sql；请在 order-core 仓里 git checkout"; exit 1; }
done

# ② 为每个 shard 拼出 "paychan_db_N + order_db_N + accounting_db_N + 平台账户 seed"
for i in 0 1 2 3 4 5 6 7 8 9; do
  out="$OUT/${i}_init.sql"
  acct_init="$ACCT_INIT_DIR/${i}_init.sql"
  acct_seed="$ACCT_INIT_DIR/${i}_init_tmp.sql"
  [[ -s "$acct_init" ]] || { echo "FATAL: 缺 $acct_init"; exit 1; }
  [[ -s "$acct_seed" ]] || { echo "FATAL: 缺 $acct_seed (平台账户 seed)"; exit 1; }
  {
    echo "-- ┌──────────────────────────────────────────────────────────────────────┐"
    echo "-- │ 共享 MySQL shard ${i} —— paychan_db_${i} + order_db_${i} + accounting_db_${i} │"
    echo "-- └──────────────────────────────────────────────────────────────────────┘"
    echo ""
    echo "-- ==== payment-channel ===="
    cat "$ROOT/payment-channel/database/paychandb/init/${i}_init.sql"
    echo ""
    echo "-- ==== order-core ===="
    cat "$ROOT/order-core/database/orderdb/init/${i}_init.sql"
    echo ""
    echo "-- ==== accounting-system schema ===="
    cat "$acct_init"
    echo ""
    echo "-- ==== accounting-system 平台账户 seed (按位编码 account_no) ===="
    cat "$acct_seed"
  } > "$out"
done

# ③ meta：把四仓的 meta init 合成一份（paychan_meta / order_meta /
#    user_merchant_meta / account_meta 互不冲突）
META_OUT="$OUT/meta_init.sql"
USER_MERCHANT_META="$ROOT/user-merchant-core/database/metadb/init/init.sql"
[[ -s "$USER_MERCHANT_META" ]] || { echo "FATAL: user-merchant-core 缺 metadb init SQL"; exit 1; }
{
  echo "-- 共享 meta —— paychan_meta + order_meta + user_merchant_meta + account_meta"
  echo ""
  echo "-- ==== payment-channel ===="
  cat "$ROOT/payment-channel/database/metadb/init/init.sql"
  echo ""
  echo "-- ==== order-core ===="
  cat "$ROOT/order-core/database/metadb/init/init.sql"
  echo ""
  echo "-- ==== user-merchant-core ===="
  cat "$USER_MERCHANT_META"
  echo ""
  echo "-- ==== accounting-system (account_meta: leaf_alloc / business_type / hot_account ...) ===="
  cat "$ACCT_META_INIT"
} > "$META_OUT"

echo "生成完毕："
echo "  $OUT/0_init.sql..9_init.sql (每个 ~$(du -b "$OUT/0_init.sql" | awk '{print $1}') bytes)"
echo "  $META_OUT"
