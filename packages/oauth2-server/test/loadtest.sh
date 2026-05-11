#!/usr/bin/env bash
# loadtest.sh — 用 vegeta 或 hey 跑 token endpoint 压测。
#
# 目标:
#   - p99 < 200ms @ 1000 rps
#   - 0 5xx
#   - rate-limit 在 20rps/client 起作用 (20 rps/client × N client → 总 N×20 rps)
#
# 安装:
#   brew install vegeta hey
#
# 用法:
#   ./loadtest.sh [host:port] [duration]
#     默认: http://localhost:8087, 30s
#
# 前置:
#   - oauth2-server 已起 + OAUTH2_DEV_SEED=1 + 已 source .env (或直接传 client_id)

set -euo pipefail
HOST="${1:-http://localhost:8087}"
DUR="${2:-30s}"
CLIENT_ID="${MER_CLIENT_ID:-mer_demo_merchant_01}"
CLIENT_SEC="${MER_CLIENT_SECRET:-dev_secret_merchant_001}"

mkdir -p /tmp/loadtest-out

# ─── case 1: 单 client 持续打 — 验 rate limit ────────────────
echo "▶ Case 1: 单 client × 50 rps × $DUR (期望 ~20rps 过, 余 throttled)"
cat > /tmp/targets-single.txt <<EOF
POST $HOST/oauth2/token
Content-Type: application/x-www-form-urlencoded
@/tmp/body-single.txt
EOF
echo "grant_type=client_credentials&client_id=$CLIENT_ID&client_secret=$CLIENT_SEC&scope=charge:write" > /tmp/body-single.txt

if command -v vegeta &>/dev/null; then
  vegeta attack -rate=50 -duration=$DUR -targets=/tmp/targets-single.txt \
    | tee /tmp/loadtest-out/single.bin | vegeta report
else
  echo "  vegeta 未装, 跳过 — brew install vegeta"
fi

# ─── case 2: 100 不同 client 并发 — 验整体吞吐 ───────────────
echo ""
echo "▶ Case 2: 用 hey 100 并发 × 1000 req"
if command -v hey &>/dev/null; then
  hey -n 1000 -c 100 -m POST \
    -d "grant_type=client_credentials&client_id=$CLIENT_ID&client_secret=$CLIENT_SEC" \
    -T "application/x-www-form-urlencoded" \
    "$HOST/oauth2/token" | head -30
else
  echo "  hey 未装, 跳过"
fi

# ─── case 3: 故意打错 secret — 看 brute force 告警表现 ────────
echo ""
echo "▶ Case 3: 错 secret × 100 (验 bad_secret 计数 + rate limit)"
for i in {1..100}; do
  curl -sS -o /dev/null -w "" -X POST "$HOST/oauth2/token" \
    -d "grant_type=client_credentials&client_id=$CLIENT_ID&client_secret=WRONG_$i" || true
done
echo "  (查 /metrics: oauth2_token_issue_total{outcome=\"bad_secret\"})"

# ─── 收集 metrics ───────────────────────────────────────────
echo ""
echo "▶ 关键指标 (实时):"
curl -sS "$HOST/metrics" | grep -E "^oauth2_(token_issue|introspect|rate_limit)" | head -20

echo ""
echo "✅ loadtest done — 详细报告在 /tmp/loadtest-out/"
