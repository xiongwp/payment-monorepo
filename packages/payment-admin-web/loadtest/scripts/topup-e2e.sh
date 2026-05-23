#!/usr/bin/env bash
# ============================================================================
# topup-e2e.sh —— 一笔 user-topup 端到端 demo
#
# 跑 packages/split-payment/cmd/topup-e2e/main.go：
#   1. resolve 5 LA → 5 sub-account (fleet routing)
#   2. 从 account_pool 取 1 个用户账户
#   3. 调 split-payment.TriggerEvent 触发 5-leg 原子记账
#   4. 验证每个 sub 的余额变动
#
# 前置：
#   ./scripts/fleet-prepare.sh --admin=http://localhost:<port>   # 注册 LA + 建 fleet
#   ./scripts/run.sh                                              # 跑过一次 bootstrap (生成 account_pool.json)
#                                                                 # 或者直接跑 bootstrap step：
#                                                                 # docker compose ... run bootstrap
#
# 用法：
#   ./scripts/topup-e2e.sh
#   ./scripts/topup-e2e.sh --admin=http://localhost:8893
#   ./scripts/topup-e2e.sh --admin=http://localhost:8893 --amount=50000  # 500 CNY
# ============================================================================

set -euo pipefail

ADMIN_HTTP="${ADMIN_HTTP:-http://localhost:8893}"
SPLIT_PAYMENT_GRPC="${SPLIT_PAYMENT_GRPC:-localhost:9098}"
CHANNEL="${CHANNEL:-alipay}"
AMOUNT="${AMOUNT:-10000}"
CURRENCY="${CURRENCY:-PHP}"

# 取 monorepo 根（脚本在 packages/payment-admin-web/loadtest/scripts/）
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
SP_DIR="${ROOT}/packages/split-payment"
POOL_FILE="${ROOT}/packages/payment-admin-web/loadtest/output/account_pool.json"

# 把直接 --foo=bar 传给底层 go 程
GO_ARGS=()
for arg in "$@"; do
  case "$arg" in
    --admin=*)         ADMIN_HTTP="${arg#*=}" ;;
    --split-payment=*) SPLIT_PAYMENT_GRPC="${arg#*=}" ;;
    --channel=*)       CHANNEL="${arg#*=}" ;;
    --amount=*)        AMOUNT="${arg#*=}" ;;
    --currency=*)      CURRENCY="${arg#*=}" ;;
    --pool=*)          POOL_FILE="${arg#*=}" ;;
    -h|--help)
      sed -n '2,/^set -euo/p' "$0" | sed 's/^# \?//'
      exit 0 ;;
    *) GO_ARGS+=("$arg") ;;
  esac
done

if [[ ! -f "${POOL_FILE}" ]]; then
  echo "ERROR: account_pool.json 不存在: ${POOL_FILE}"
  echo "       先跑 ./scripts/run.sh 让 loadtest 生成账户池，或手动跑 bootstrap"
  exit 1
fi

cd "${SP_DIR}"

# 关闭 GOWORK，避免读到 monorepo 根的 go.work
export GOWORK=off

exec go run ./cmd/topup-e2e \
  --admin="${ADMIN_HTTP}" \
  --split-payment="${SPLIT_PAYMENT_GRPC}" \
  --channel="${CHANNEL}" \
  --amount="${AMOUNT}" \
  --currency="${CURRENCY}" \
  --pool="${POOL_FILE}" \
  ${GO_ARGS[@]+"${GO_ARGS[@]}"}
