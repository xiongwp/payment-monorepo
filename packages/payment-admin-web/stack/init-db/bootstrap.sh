#!/usr/bin/env bash
# init-db/bootstrap.sh — 把所有服务的 schema 灌进 11 个 MySQL 实例。
#
# 拓扑（与 docker-compose 对齐）：
#   shared-meta              → user_merchant_meta / paychan_meta / order_meta /
#                              account_meta（4 个非分片 meta DB）
#   shared-shard-N (0..9)    → paychan_db_N / order_db_N / accounting_db_N
#                              （每个分片实例装 3 个同序号的 sharded DB）
#
# 灌库流程：
#   1. shared-meta：依次跑 user_merchant_meta / paychan_meta / order_meta init.sql
#                  account_meta 的 schema 由 accounting-system server 启动期建表
#   2. 对每个 shared-shard-N：跑 paychan_db_N / order_db_N / accounting_db_N init.sql
#
# 所有 SQL 均 CREATE * IF NOT EXISTS，重复执行安全。
set -euo pipefail

DB_USER="${SHARED_DB_USER:-root}"
DB_PASS="${SHARED_DB_PASS:-password}"

apply() {
    local host="$1" label="$2" file="$3"
    if [ ! -f "$file" ]; then
        echo "[skip] $label: $file not found"
        return 0
    fi
    echo "[apply] $host ← $label ($file)"
    mysql -h"$host" -P3306 -u"$DB_USER" -p"$DB_PASS" < "$file"
}

wait_ready() {
    local host="$1"
    echo "waiting for $host:3306 ..."
    for _ in $(seq 1 60); do
        if mysql -h"$host" -P3306 -u"$DB_USER" -p"$DB_PASS" -e "SELECT 1" >/dev/null 2>&1; then
            echo "$host ready"
            return 0
        fi
        sleep 2
    done
    echo "$host did not become ready in 120s" >&2
    return 1
}

# ─── shared-meta ──────────────────────────────────────────────────────────
wait_ready shared-meta
apply shared-meta "user-merchant-core/meta" /sql/user-merchant-core/database/metadb/init/init.sql
apply shared-meta "payment-channel/meta"    /sql/payment-channel/database/metadb/init/init.sql
apply shared-meta "order-core/meta"         /sql/order-core/database/metadb/init/init.sql
# accounting-system 的 account_meta 包含 account_type_info / transaction_rule /
# leaf_alloc / hot_account / account_business_type_info 等，必须预灌。
# 不灌的话 accounting-system idgen 启动就报 Table 'account_meta.leaf_alloc' doesn't exist。
apply shared-meta "accounting-system/meta"  /sql/accounting-system/database/metadb/init/init.sql
# card-center / card-payment 的 meta（dev 模式下与其它 meta 同居 shared-meta；
# 生产 SAQ-D 严格要求独立 DC，由 env=prod assertProdSafety 强制不同 DSN）。
apply shared-meta "card-center/meta"        /sql/card-center/database/metadb/init/init.sql
apply shared-meta "card-payment/meta"       /sql/card-payment/database/metadb/init/init.sql

# ─── shared-shard-0 .. shared-shard-9 ─────────────────────────────────────
for i in $(seq 0 9); do
    host="shared-shard-$i"
    wait_ready "$host"
    apply "$host" "payment-channel/db_$i"          "/sql/payment-channel/database/paychandb/init/${i}_init.sql"
    apply "$host" "order-core/db_$i"               "/sql/order-core/database/orderdb/init/${i}_init.sql"
    apply "$host" "accounting-system/db_$i"        "/sql/accounting-system/database/accountingdb/init/${i}_init.sql"
    # _tmp.sql 含预置的平台账户 seed INSERT（000_PLATFORM_PROFIT_REVENUE 等），
    # 不灌就没有 fleet，admin-web 平台账户页会 "已找到 0 个账户"。
    apply "$host" "accounting-system/db_${i}_seed" "/sql/accounting-system/database/accountingdb/init/${i}_init_tmp.sql"
    # card-center: card_center_db_$i (10 张 sharded 表 / 库)
    apply "$host" "card-center/db_$i"              "/sql/card-center/database/userdb/init/${i}_init.sql"
    # card-payment: card_payment_db_$i (10 张 sharded 表 / 库)
    apply "$host" "card-payment/db_$i"             "/sql/card-payment/database/cardpaymentdb/init/${i}_init.sql"
done

echo "[done] all schemas applied across 1 meta + 10 shard MySQL instances"
