#!/usr/bin/env bash
# ============================================================================
# 预注册 logical_accounts + rotation policy + split-payment graph
#
# accounting-system 不再 lazy-create logical_account（设计文档 §3）：
# 业务方必须在第一次 booking 前显式 RegisterLogicalAccount，否则 booking 拒绝。
#
# 本脚本在压测前注册：
#   1. 4 类中间账户的 logical_account（channel-payable / channel-receivable /
#      user-suspense / merchant-suspense），按渠道 + 币种维度
#   2. 给每个 LA 配置 rotation policy（默认 1 个月切换）
#   3. 通知 split-payment 加载 4 个 graph（user-topup-v1 等），如果还没注册
#
# 调用：docker compose exec -T loadtest bash bootstrap-logical-accounts.sh
# 或   bash scripts/bootstrap-logical-accounts.sh     （宿主机直接打 admin http）
# ============================================================================

set -euo pipefail

# ─── 配置 ─────────────────────────────────────────────────────────────────
ACCOUNTING_ADMIN="${ACCOUNTING_ADMIN:-http://localhost:19094}"   # compose 宿主端口（19092 已被 kafka 占）
SPLIT_PAYMENT_ADMIN="${SPLIT_PAYMENT_ADMIN:-http://localhost:19099}"
ADMIN_TOKEN="${ADMIN_TOKEN:-}"
CURRENCY="${CURRENCY:-PHP}"
CHANNEL_COUNT="${CHANNEL_COUNT:-100}"
ROTATION_PERIOD="${ROTATION_PERIOD:-1month}"   # 1month / 3month / 7day（业务自定义）

# 鉴权 header（如配了 token）
HEADER_ARGS=()
if [[ -n "${ADMIN_TOKEN}" ]]; then
  HEADER_ARGS=(-H "X-Admin-Token: ${ADMIN_TOKEN}")
fi

# ─── 1. 健康检查 ──────────────────────────────────────────────────────────
echo ">>> 健康检查 accounting-system: ${ACCOUNTING_ADMIN}"
if ! curl -sf "${ACCOUNTING_ADMIN}/admin/health" >/dev/null; then
  echo "ERROR: accounting-system 不可达。检查 docker compose ps" >&2
  exit 1
fi

# ─── 2. 注册 4 类中间账户的 logical_account ──────────────────────────────
# logical_account_key 命名约定：{purpose}:{currency}[:{scope_id}]
#   purpose ∈ channel-payable / channel-receivable / user-suspense / merchant-suspense
#   scope_id = channel_id 或 leave empty for global
register_la() {
  local key="$1"
  local account_type="$2"   # 10/11/12/13 见 rotation.go AccountBusinessType
  local rotation_enabled="${3:-true}"

  # NOTE: accounting-system 当前没有 HTTP 端点直接注册 logical_account
  # （rotation_admin_service 是 Go service 层 + Kitex gRPC 调用）；
  # 这里用 /admin/rotation/register-la（见 admin-web 接入；如未实现，跳过）
  resp=$(curl -s -w "\n%{http_code}" -X POST "${ACCOUNTING_ADMIN}/admin/rotation/register-la" \
    "${HEADER_ARGS[@]}" \
    -H "Content-Type: application/json" \
    -d "{
      \"logical_account_key\": \"${key}\",
      \"account_type\": ${account_type},
      \"currency\": \"${CURRENCY}\",
      \"rotation_enabled\": ${rotation_enabled},
      \"rotation_period\": \"${ROTATION_PERIOD}\"
    }" 2>/dev/null || true)
  code=$(echo "${resp}" | tail -n1)
  body=$(echo "${resp}" | sed '$d')
  case "${code}" in
    200|201) echo "    [+] ${key} (type=${account_type}) 已注册" ;;
    409)     echo "    [=] ${key} 已存在，跳过" ;;
    404)     echo "    [!] /admin/rotation/register-la 端点未实现；请通过 gRPC 注册或加这个端点"; return 0 ;;
    *)       echo "    [x] ${key} 注册失败 (HTTP ${code}): ${body}" ;;
  esac
}

echo ">>> 注册中间账户 (currency=${CURRENCY})"
for i in $(seq 1 "${CHANNEL_COUNT}"); do
  channel_id=$(printf "ch-%03d" "$i")
  register_la "channel-payable:${CURRENCY}:${channel_id}"    10  # MigrationSuspense
  register_la "channel-receivable:${CURRENCY}:${channel_id}" 12  # RotationOpsAdjust
done
register_la "user-suspense:${CURRENCY}"     13     # RotationCarryforward
register_la "merchant-suspense:${CURRENCY}" 13

# ─── 3. 注册 split-payment graph（4 个资金流）─────────────────────────
# split-payment 用 SaveGraph 加载/更新规则
# 这里假设 4 个 graph 文件存在于 /loadtest/graphs/*.json，由 deploy 时挂入
register_graph() {
  local graph_key="$1"
  local graph_path="$2"
  if [[ ! -f "${graph_path}" ]]; then
    echo "    [!] graph 文件 ${graph_path} 不存在，跳过 ${graph_key}（请用 admin-web 手动配置）"
    return 0
  fi
  echo "    [+] 注册 graph ${graph_key} ← ${graph_path}"
  # split-payment 用 gRPC SaveGraph，这里用 admin HTTP 触发同等行为
  curl -sf -X POST "${SPLIT_PAYMENT_ADMIN}/admin/graphs/save" \
    "${HEADER_ARGS[@]}" \
    -H "Content-Type: application/json" \
    -d "@${graph_path}" \
    || echo "    [x] save graph ${graph_key} 失败（可能 admin HTTP 端点没暴露这个，需用 grpcurl）"
}

echo ">>> 注册 split-payment graph"
GRAPH_DIR="${GRAPH_DIR:-/loadtest/graphs}"
register_graph "user-topup-v1"             "${GRAPH_DIR}/topup.json"
register_graph "user-balance-payment-v1"   "${GRAPH_DIR}/payment.json"
register_graph "user-internal-transfer-v1" "${GRAPH_DIR}/transfer.json"
register_graph "user-withdraw-v1"          "${GRAPH_DIR}/withdraw.json"

echo ">>> bootstrap 完成"
