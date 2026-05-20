#!/usr/bin/env bash
# e2e-smoke.sh — 端到端冒烟测试: 起栈 → 触发 charge → 验账户余额.
#
# 跑得过的前置:
#   1. ./deploy-stack.sh up --rebuild  (或至少 accounting-system / order-core / api-gateway / user-merchant-core 跑着)
#   2. ./deploy-stack.sh seed          (config-center 种子 + accounting transaction_rule)
#   3. shared-meta MySQL 监听 127.0.0.1:3308 (或 export DB_HOST=...)
#
# 跑完后检查:
#   - api-gateway 接受 charge → 201
#   - order-core outbox 把事件投到 accounting → transaction_order.status=success
#   - accounting.account 余额按 amount 变动 (channel-buffer +amount, merchant-pending +amount)
#
# 退出码: 0 = 全部 PASS, 1 = 任一断言失败.

set -u

ROOT="$(cd "$(dirname "$0")" && pwd)"
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
ok()   { echo -e "${GREEN}✓${NC} $*"; }
fail() { echo -e "${RED}✗${NC} $*" >&2; exit 1; }
warn() { echo -e "${YELLOW}!${NC} $*"; }

# ─── 配置 ─────────────────────────────────────────────────────────────────
GW_URL="${GW_URL:-http://localhost:8080}"
DB_HOST="${DB_HOST:-127.0.0.1}"
DB_PORT="${DB_PORT:-3308}"
DB_USER="${DB_USER:-root}"
DB_PASS="${DB_PASS:-root}"
MERCHANT_ID="${MERCHANT_ID:-1001}"
CURRENCY="${CURRENCY:-PHP}"
AMOUNT_MINOR="${AMOUNT_MINOR:-12345}"   # 123.45 PHP
PAYMENT_METHOD="${PAYMENT_METHOD:-stripe}"
RUN_ID="e2e-$(date +%s)-$$"

mysql_q() { mysql -h "$DB_HOST" -P "$DB_PORT" -u "$DB_USER" -p"$DB_PASS" -sN -e "$1"; }

# ─── 1. health checks ────────────────────────────────────────────────────
echo
echo "Step 1: health checks"
curl -fs "$GW_URL/healthz" >/dev/null 2>&1 || fail "api-gateway $GW_URL/healthz 不通"
ok "api-gateway 健康"
mysql_q "SELECT 1" >/dev/null 2>&1 || fail "MySQL $DB_HOST:$DB_PORT 连不上"
ok "MySQL 连通"

# ─── 2. 余额快照 ──────────────────────────────────────────────────────────
echo
echo "Step 2: 余额快照 (before)"
BEFORE_MERCH=$(mysql_q "SELECT IFNULL(SUM(balance),0) FROM accounting.account WHERE user_id=$MERCHANT_ID AND account_business_type=3 AND currency='$CURRENCY'")
BEFORE_CHAN=$(mysql_q "SELECT IFNULL(SUM(balance),0) FROM accounting.account WHERE account_type=4 AND currency='$CURRENCY' AND description LIKE '%$PAYMENT_METHOD%' LIMIT 1")
echo "  merchant pending-settle (user=$MERCHANT_ID): $BEFORE_MERCH minor units"
echo "  channel buffer ($PAYMENT_METHOD):            $BEFORE_CHAN minor units"

# ─── 3. 触发 charge ──────────────────────────────────────────────────────
echo
echo "Step 3: POST $GW_URL/v1/charges  amount=$AMOUNT_MINOR run_id=$RUN_ID"
RESP=$(curl -sS -X POST "$GW_URL/v1/charges" \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: $RUN_ID" \
    -d "{
      \"merchant_id\": \"$MERCHANT_ID\",
      \"amount\": $AMOUNT_MINOR,
      \"currency\": \"$CURRENCY\",
      \"payment_method\": \"$PAYMENT_METHOD\",
      \"return_url\": \"https://example.test/return\"
    }")
echo "  response: $RESP"
CHARGE_ID=$(echo "$RESP" | grep -oE '"charge_id"\s*:\s*"[^"]+"' | head -1 | cut -d'"' -f4)
[[ -n "$CHARGE_ID" ]] || fail "charge_id 没拿到, response 异常: $RESP"
ok "charge created: $CHARGE_ID"

# ─── 4. 等 outbox flush + accounting 落账 ────────────────────────────────
echo
echo "Step 4: 等 outbox worker 投递 (最多 30s)"
for i in $(seq 1 30); do
  STATUS=$(mysql_q "SELECT status FROM order_core.accounting_outbox WHERE charge_id='$CHARGE_ID' LIMIT 1" 2>/dev/null || echo "")
  if [[ "$STATUS" == "sent" ]]; then
    ok "outbox status=sent (耗时 ${i}s)"
    break
  fi
  sleep 1
done
[[ "$STATUS" == "sent" ]] || warn "outbox 未在 30s 内 sent (status=$STATUS); 继续检查 transaction_order"

# ─── 5. 验 transaction_order ─────────────────────────────────────────────
echo
echo "Step 5: accounting.transaction_order 落库验证"
TX_ROW=$(mysql_q "SELECT order_no, business_type, status, voucher_no FROM accounting.transaction_order_00 WHERE business_no='$CHARGE_ID' UNION ALL SELECT order_no, business_type, status, voucher_no FROM accounting.transaction_order_01 WHERE business_no='$CHARGE_ID' LIMIT 1")
[[ -n "$TX_ROW" ]] || fail "transaction_order 没找到 business_no=$CHARGE_ID 的行 (注: 表名按 business_no 后 2 位分片, 这里查了 00/01, 实际生产要扫全 100 张表)"
ok "transaction_order row: $TX_ROW"

# ─── 6. 验账户余额变化 ──────────────────────────────────────────────────
echo
echo "Step 6: 余额变化校验 (after)"
AFTER_MERCH=$(mysql_q "SELECT IFNULL(SUM(balance),0) FROM accounting.account WHERE user_id=$MERCHANT_ID AND account_business_type=3 AND currency='$CURRENCY'")
AFTER_CHAN=$(mysql_q "SELECT IFNULL(SUM(balance),0) FROM accounting.account WHERE account_type=4 AND currency='$CURRENCY' AND description LIKE '%$PAYMENT_METHOD%' LIMIT 1")
DIFF_MERCH=$((AFTER_MERCH - BEFORE_MERCH))
DIFF_CHAN=$((AFTER_CHAN - BEFORE_CHAN))
echo "  merchant pending-settle: $BEFORE_MERCH → $AFTER_MERCH (Δ=$DIFF_MERCH)"
echo "  channel buffer:          $BEFORE_CHAN → $AFTER_CHAN (Δ=$DIFF_CHAN)"

if [[ "$DIFF_MERCH" -eq "$AMOUNT_MINOR" && "$DIFF_CHAN" -eq "$AMOUNT_MINOR" ]]; then
  ok "余额变动正确 (双方各 +$AMOUNT_MINOR)"
else
  fail "余额变动不对 (期望 +$AMOUNT_MINOR 两边; 实际 merch=$DIFF_MERCH chan=$DIFF_CHAN)"
fi

echo
echo "═══════════════════════════════════════════════"
ok "e2e smoke PASS  (charge_id=$CHARGE_ID amount=$AMOUNT_MINOR $CURRENCY)"
echo "═══════════════════════════════════════════════"
