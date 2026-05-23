#!/usr/bin/env bash
# ============================================================================
# fleet-prepare.sh —— Fleet × Rotation 压测前置准备
#
# 给 loadtest 用的 LA 注册 + fleet 预创建脚本。运行后：
#   1. 注册每个白名单 prefix 的 LA（如 channel-payable:alipay, channel-fee:alipay...）
#   2. 触发 manual-provision 让 fleet 100 sub 全部进 provisioned
#   3. 触发 manual-switch 把 100 sub 提到 active
#
# 后续 split-payment loadtest 调 accounting 时填 LogicalAccountKey + FlowID，
# accounting 自动按 fnv32a(flow_id)%100 选 sub_idx → 流量散到 100 个物理账户。
#
# 用法：
#   ./scripts/fleet-prepare.sh                          # 默认 alipay 渠道
#   ./scripts/fleet-prepare.sh --channels alipay,wechat # 多渠道
#
# 前置：accounting-service 已起、admin HTTP 通（默认 host port 8891）。
# ============================================================================

set -euo pipefail

# 自动探测 accounting admin port — 每次重启 docker compose 容器端口会飘
# (compose 在 8890-8897 range 内顺移分配)。直接问 docker 拿现在的 host port。
autodetect_admin_port() {
  local p
  p=$(docker port accounting-system-accounting-service-1 8888/tcp 2>/dev/null | head -1 | awk -F: '{print $NF}')
  if [[ -z "$p" ]]; then
    p=$(docker port accounting-system-accounting-service-2 8888/tcp 2>/dev/null | head -1 | awk -F: '{print $NF}')
  fi
  echo "$p"
}
DETECTED_PORT=$(autodetect_admin_port)
if [[ -n "$DETECTED_PORT" ]]; then
  ADMIN_HTTP="${ADMIN_HTTP:-http://localhost:${DETECTED_PORT}}"
else
  ADMIN_HTTP="${ADMIN_HTTP:-http://localhost:8891}"   # 兜底（容器不在跑就用 default 让用户看到清晰错误）
fi
OPERATOR="${OPERATOR:-loadtest-fleet-prepare}"
REASON="${REASON:-prepare fleet for loadtest}"
CURRENCY="${CURRENCY:-PHP}"

# 渠道列表（默认 alipay；多个用逗号分隔）
CHANNELS="alipay"

# 9 个白名单 prefix。其中渠道相关（前 4 个）每个渠道单独建 LA；平台/通用类
# (后 5 个) 不带渠道，全局一份。
CHANNEL_PREFIXES=(
  "channel-receivable:"
  "channel-payable:"
  "channel-suspense:"
  "channel-fee:"
)
PLATFORM_PREFIXES=(
  "platform-fee-clearing:"
  "platform-fee-revenue:"
  "platform-withdraw-pending:"
  "user-suspense:"
  "transit:"
)

# prefix → 推荐 account_type（跟 UI KEY_PREFIX_OPTIONS 一致）
# 不用 declare -A，因为 macOS 默认 bash 3.2 不支持 associative array。
account_type_for_prefix() {
  case "$1" in
    "channel-receivable:")        echo 5 ;;
    "channel-payable:")           echo 6 ;;
    "channel-suspense:")          echo 9 ;;
    "channel-fee:")               echo 7 ;;
    "platform-fee-clearing:")     echo 4 ;;
    "platform-fee-revenue:")      echo 4 ;;
    "platform-withdraw-pending:") echo 9 ;;
    "user-suspense:")             echo 9 ;;
    "transit:")                   echo 9 ;;
    *)                            echo 9 ;;
  esac
}

# account_business_type 用 1（GENERAL）即可
ABT=1

# ─── 参数解析 ─────────────────────────────────────────────────────────────
for arg in "$@"; do
  case "$arg" in
    --channels=*) CHANNELS="${arg#*=}" ;;
    --channels)   shift; CHANNELS="$1" ;;
    --currency=*) CURRENCY="${arg#*=}" ;;
    --admin=*)    ADMIN_HTTP="${arg#*=}" ;;
    -h|--help)
      sed -n '2,/^set -euo/p' "$0" | sed 's/^# \?//'
      exit 0 ;;
  esac
done

green() { printf "\033[32m%s\033[0m\n" "$*"; }
yellow(){ printf "\033[33m%s\033[0m\n" "$*"; }
red()   { printf "\033[31m%s\033[0m\n" "$*"; }

# 注册 + provision + switch 单个 LA。幂等：已存在的 LA register 返回 409，
# 这里把 409 视为 OK 继续走 provision + switch（manual-provision 会建下一期
# fleet，已 active 时这会建第二代）。
register_and_provision() {
  local key="$1"
  local account_type="$2"

  echo
  yellow "[LA] ${key}"

  # ── 1. register（已存在 → 409 → 跳过）
  local register_payload
  register_payload=$(cat <<EOF
{
  "logical_account_key": "${key}",
  "account_type": ${account_type},
  "account_business_type": ${ABT},
  "currency": "${CURRENCY}",
  "rotation_enabled": true,
  "operator": "${OPERATOR}"
}
EOF
)
  local rc
  : > /tmp/reg_resp   # 清掉上一次的残留，避免 HTTP 000 时打印错的消息
  rc=$(curl -s -o /tmp/reg_resp -w "%{http_code}" -X POST \
    "${ADMIN_HTTP}/admin/rotation/register" \
    -H 'Content-Type: application/json' \
    -d "${register_payload}")
  case "${rc}" in
    200)  green "  ✓ registered (new)" ;;
    409)  yellow "  • already exists, skip register" ;;
    000)  red "  ✗ connection refused (admin HTTP ${ADMIN_HTTP} unreachable)"; return 1 ;;
    *)    red "  ✗ register failed (HTTP ${rc}): $(cat /tmp/reg_resp 2>/dev/null)"; return 1 ;;
  esac

  # ── 2. manual-provision（建 100 sub）
  local op_payload
  op_payload=$(cat <<EOF
{"logical_account_key": "${key}", "operator": "${OPERATOR}", "reason": "${REASON}"}
EOF
)
  : > /tmp/prov_resp
  rc=$(curl -s -o /tmp/prov_resp -w "%{http_code}" -X POST \
    "${ADMIN_HTTP}/admin/rotation/manual-provision" \
    -H 'Content-Type: application/json' \
    -d "${op_payload}")
  if [[ "${rc}" -ne 200 ]]; then
    red "  ✗ manual-provision failed (HTTP ${rc}): $(cat /tmp/prov_resp 2>/dev/null)"
    return 1
  fi
  green "  ✓ provisioned (100 sub)"

  # ── 3. manual-switch（active → draining 同时 provisioned → active）
  # 首次 active==nil 时 switch 也走 swap 路径，把 provisioned 提到 active
  : > /tmp/sw_resp
  rc=$(curl -s -o /tmp/sw_resp -w "%{http_code}" -X POST \
    "${ADMIN_HTTP}/admin/rotation/manual-switch" \
    -H 'Content-Type: application/json' \
    -d "${op_payload}")
  if [[ "${rc}" -ne 200 ]]; then
    red "  ✗ manual-switch failed (HTTP ${rc}): $(cat /tmp/sw_resp 2>/dev/null)"
    return 1
  fi
  green "  ✓ switched to active"

  # ── 4. summary verify
  local summary
  summary=$(curl -s "${ADMIN_HTTP}/admin/rotation/balance-summary?logical_account_key=${key}")
  yellow "  summary: ${summary}"
}

# ─── 主流程 ─────────────────────────────────────────────────────────────
green "=========================================================="
green "Fleet × Rotation 压测准备  (ADMIN=${ADMIN_HTTP})"
green "  currency: ${CURRENCY}"
green "  channels: ${CHANNELS}"
green "=========================================================="

IFS=',' read -ra CHAN_ARR <<< "${CHANNELS}"

# 渠道维度：每个渠道 × 4 prefix = 4 个 LA
for chan in "${CHAN_ARR[@]}"; do
  chan="${chan// /}"
  [[ -z "${chan}" ]] && continue
  for p in "${CHANNEL_PREFIXES[@]}"; do
    key="${p}${chan}"
    register_and_provision "${key}" "$(account_type_for_prefix "${p}")" || true
  done
done

# 平台/通用：每个 prefix 一个 LA（用 "default" 后缀，可按需扩展）
for p in "${PLATFORM_PREFIXES[@]}"; do
  key="${p}default"
  register_and_provision "${key}" "$(account_type_for_prefix "${p}")" || true
done

green ""
green "✅ Fleet 准备完成。下一步："
green "   1. 确保 split-payment SPLIT_PAYMENT_FLEET_ROUTING_ENABLED=true 已经设置（deploy/overrides/split-payment.yml 已默认开）"
green "   2. 跑常规 loadtest：./scripts/run.sh"
green "   3. 跑完看 sub 分布：./scripts/fleet-verify.sh"
