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
USER_MERCHANT_GEN="$ROOT/user-merchant-core/database/userdb/scripts/generate.sh"
ACCT_INIT_DIR="$ROOT/accounting-system/database/accountingdb/init"
ACCT_META_INIT="$ROOT/accounting-system/database/metadb/init/init.sql"
# PCI 卡支付 dev 联栈（生产必须独立 DC，不走这条）
CARD_CENTER_GEN="$ROOT/card-center/database/userdb/scripts/generate.sh"
CARD_PAYMENT_GEN="$ROOT/card-payment/database/cardpaymentdb/scripts/generate.sh"
CARD_CENTER_META_INIT="$ROOT/card-center/database/metadb/init/init.sql"
CARD_PAYMENT_META_INIT="$ROOT/card-payment/database/metadb/init/init.sql"

[[ -x "$PAYCHAN_GEN"        ]] || { echo "FATAL: $PAYCHAN_GEN 不存在或不可执行"; exit 1; }
[[ -x "$ORDER_GEN"          ]] || { echo "FATAL: $ORDER_GEN 不存在或不可执行"; exit 1; }
[[ -x "$USER_MERCHANT_GEN"  ]] || { echo "FATAL: $USER_MERCHANT_GEN 不存在或不可执行"; exit 1; }
[[ -d "$ACCT_INIT_DIR"      ]] || { echo "FATAL: $ACCT_INIT_DIR 不存在（accounting-system 仓需平级）"; exit 1; }
[[ -s "$ACCT_META_INIT"     ]] || { echo "FATAL: $ACCT_META_INIT 不存在"; exit 1; }

# ① 确保所有仓各自的 per-shard init SQL 已生成
(cd "$ROOT/payment-channel"    && rm -rf database/paychandb/init    && bash database/paychandb/scripts/generate.sh) >/dev/null
(cd "$ROOT/user-merchant-core" && rm -rf database/userdb/init       && bash database/userdb/scripts/generate.sh)    >/dev/null
# card-center / card-payment：dev 联栈也得拼进 shared-db 才能起服务
if [[ -x "$CARD_CENTER_GEN" ]]; then
  (cd "$ROOT/card-center"  && rm -rf database/userdb/init        && bash database/userdb/scripts/generate.sh) >/dev/null
fi
if [[ -x "$CARD_PAYMENT_GEN" ]]; then
  (cd "$ROOT/card-payment" && rm -rf database/cardpaymentdb/init && bash database/cardpaymentdb/scripts/generate.sh) >/dev/null
fi
# order-core 的 init 已在 git 里；只在没有/是目录时重拉一次
for i in 0 1 2 3 4 5 6 7 8 9; do
  f="$ROOT/order-core/database/orderdb/init/${i}_init.sql"
  if [[ -d "$f" || ! -s "$f" ]]; then
    (cd "$ROOT/order-core" && rm -rf database/orderdb/init && git checkout database/orderdb/init) 2>/dev/null || true
  fi
  [[ -s "$f" ]] || { echo "FATAL: order-core 缺 ${i}_init.sql；请在 order-core 仓里 git checkout"; exit 1; }
done

# ② 为每个 shard 拼出 "paychan_db_N + order_db_N + accounting_db_N +
#    user_merchant_db_N + 平台账户 seed + 各仓 _shadow"
for i in 0 1 2 3 4 5 6 7 8 9; do
  out="$OUT/${i}_init.sql"
  acct_init="$ACCT_INIT_DIR/${i}_init.sql"
  acct_seed="$ACCT_INIT_DIR/${i}_init_tmp.sql"
  user_merchant_init="$ROOT/user-merchant-core/database/userdb/init/${i}_init.sql"
  paychan_shadow="$ROOT/payment-channel/database/paychandb/init/${i}_init_shadow.sql"
  order_shadow="$ROOT/order-core/database/orderdb/init/${i}_init_shadow.sql"
  user_merchant_shadow="$ROOT/user-merchant-core/database/userdb/init/${i}_init_shadow.sql"
  acct_shadow="$ACCT_INIT_DIR/${i}_init_shadow.sql"
  [[ -s "$acct_init" ]]          || { echo "FATAL: 缺 $acct_init"; exit 1; }
  [[ -s "$acct_seed" ]]          || { echo "FATAL: 缺 $acct_seed (平台账户 seed)"; exit 1; }
  [[ -s "$user_merchant_init" ]] || { echo "FATAL: 缺 $user_merchant_init"; exit 1; }
  {
    echo "-- ┌──────────────────────────────────────────────────────────────────────┐"
    echo "-- │ 共享 MySQL shard ${i} —— paychan_db_${i} + order_db_${i} +"
    echo "-- │ accounting_db_${i} + user_merchant_db_${i}                            │"
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
    echo ""
    echo "-- ==== user-merchant-core ===="
    cat "$user_merchant_init"
    # ── 影子表（按文件存在性可选追加；缺失不致命）──
    if [[ -s "$paychan_shadow" ]]; then
      echo ""
      echo "-- ==== payment-channel _shadow ===="
      cat "$paychan_shadow"
    fi
    if [[ -s "$order_shadow" ]]; then
      echo ""
      echo "-- ==== order-core _shadow ===="
      cat "$order_shadow"
    fi
    if [[ -s "$acct_shadow" ]]; then
      echo ""
      echo "-- ==== accounting-system _shadow ===="
      cat "$acct_shadow"
    fi
    if [[ -s "$user_merchant_shadow" ]]; then
      echo ""
      echo "-- ==== user-merchant-core _shadow ===="
      cat "$user_merchant_shadow"
    fi
    # card-center / card-payment：可选，生成器存在就拼，缺失也不致命（生产 SAQ-D 不走这条）
    cc_init="$ROOT/card-center/database/userdb/init/${i}_init.sql"
    cp_init="$ROOT/card-payment/database/cardpaymentdb/init/${i}_init.sql"
    cc_shadow="$ROOT/card-center/database/userdb/init/${i}_init_shadow.sql"
    cp_shadow="$ROOT/card-payment/database/cardpaymentdb/init/${i}_init_shadow.sql"
    if [[ -s "$cc_init" ]]; then
      echo ""
      echo "-- ==== card-center ===="
      cat "$cc_init"
    fi
    if [[ -s "$cp_init" ]]; then
      echo ""
      echo "-- ==== card-payment ===="
      cat "$cp_init"
    fi
    if [[ -s "$cc_shadow" ]]; then
      echo ""
      echo "-- ==== card-center _shadow ===="
      cat "$cc_shadow"
    fi
    if [[ -s "$cp_shadow" ]]; then
      echo ""
      echo "-- ==== card-payment _shadow ===="
      cat "$cp_shadow"
    fi
  } > "$out"
done

# ③ meta：把四仓的 meta init 合成一份（paychan_meta / order_meta /
#    user_merchant_meta / account_meta 互不冲突），各仓 _shadow 紧随其后。
META_OUT="$OUT/meta_init.sql"
USER_MERCHANT_META="$ROOT/user-merchant-core/database/metadb/init/init.sql"
USER_MERCHANT_META_SHADOW="$ROOT/user-merchant-core/database/metadb/init/init_shadow.sql"
PAYCHAN_META_SHADOW="$ROOT/payment-channel/database/metadb/init/init_shadow.sql"
ORDER_META_SHADOW="$ROOT/order-core/database/metadb/init/init_shadow.sql"
ACCT_META_SHADOW="$ROOT/accounting-system/database/metadb/init/init_shadow.sql"
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
  # card-center / card-payment meta（dev 联栈复用 shared-meta；生产独立 DC）
  if [[ -s "$CARD_CENTER_META_INIT" ]]; then
    echo ""
    echo "-- ==== card-center meta (card_center_meta: leaf_alloc + audit_log) ===="
    cat "$CARD_CENTER_META_INIT"
  fi
  if [[ -s "$CARD_PAYMENT_META_INIT" ]]; then
    echo ""
    echo "-- ==== card-payment meta (card_payment_meta) ===="
    cat "$CARD_PAYMENT_META_INIT"
  fi
  # ── meta 影子表（按文件存在性可选追加；依赖前面主表已建）──
  for shadow in "$PAYCHAN_META_SHADOW" "$ORDER_META_SHADOW" "$USER_MERCHANT_META_SHADOW" "$ACCT_META_SHADOW"; do
    if [[ -s "$shadow" ]]; then
      echo ""
      echo "-- ==== $(basename $(dirname $(dirname $(dirname "$shadow")))) meta _shadow ===="
      cat "$shadow"
    fi
  done
} > "$META_OUT"

echo "生成完毕："
echo "  $OUT/0_init.sql..9_init.sql (每个 ~$(du -b "$OUT/0_init.sql" | awk '{print $1}') bytes)"
echo "  $META_OUT"
