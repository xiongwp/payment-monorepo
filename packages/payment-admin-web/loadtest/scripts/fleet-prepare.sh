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

# account_business_type 必须跟 account_type 1:1 对齐(accounting platformAccountSpec
# 强制约定),否则 ForceProvision 建出来的 100 个 sub-account 全部 biz=1,
# 试算平衡分类明细按业务类型摊开时全压成一行,跟实时试算对不上。
# 映射跟 packages/accounting-system/internal/service/accounting_service.go 里的
# platformAccountSpec 完全一致(为了不发版改 yaml 才用 shell case)。
business_type_for_prefix() {
  case "$1" in
    "channel-receivable:")        echo 5 ;;  # TRANSIT_CHANNEL_RECEIVABLE
    "channel-payable:")           echo 6 ;;  # TRANSIT_CHANNEL_PAYABLE
    "channel-suspense:")          echo 9 ;;  # TRANSIT(中间账户)
    "channel-fee:")               echo 7 ;;  # TRANSACTION_FEE
    "platform-fee-clearing:")     echo 4 ;;  # PLATFORM_PNL
    "platform-fee-revenue:")      echo 4 ;;  # PLATFORM_PNL
    "platform-withdraw-pending:") echo 9 ;;  # TRANSIT
    "user-suspense:")             echo 9 ;;  # TRANSIT
    "transit:")                   echo 9 ;;  # TRANSIT
    *)                            echo 9 ;;
  esac
}

# ─── 参数解析 ─────────────────────────────────────────────────────────────
#
# --register-biz 是可重复 flag,启动时先把额外 biz_type 注册到
# accounting 的 account_business_type_info(POST /admin/business-types),
# 然后再走标准 LA register + provision。预置 biz=1..9 已在 init.sql 里,
# 本 flag 只用于扩展(比如新增渠道独占的 biz_type=101 等)。
#
# 格式: --register-biz=CODE:account_type[:business_type[:description]]
#   CODE             唯一码名(英文大写下划线),如 ALIPAY_PREMIUM_RECEIVABLE
#   account_type     1-9(必填,跟 accounting 的 spec 对应:5 渠道应收 / 6 应付 /
#                    7 手续费 / 9 中间 / 4 平台损益 ...)
#   business_type    数字码;留空 / 0 → 让服务端在 [101, 999] 自动分配(推荐)
#   description      可选描述
#
# 示例:
#   ./fleet-prepare.sh \
#     --register-biz=ALIPAY_PREMIUM_RECV:5 \
#     --register-biz=WECHAT_VIP_FEE:7:201:WeChat VIP 手续费
BIZ_TYPES_TO_REGISTER=()
for arg in "$@"; do
  case "$arg" in
    --channels=*)     CHANNELS="${arg#*=}" ;;
    --channels)       shift; CHANNELS="$1" ;;
    --currency=*)     CURRENCY="${arg#*=}" ;;
    --admin=*)        ADMIN_HTTP="${arg#*=}" ;;
    --register-biz=*) BIZ_TYPES_TO_REGISTER+=("${arg#*=}") ;;
    -h|--help)
      sed -n '2,/^set -euo/p' "$0" | sed 's/^# \?//'
      exit 0 ;;
  esac
done

green() { printf "\033[32m%s\033[0m\n" "$*"; }
yellow(){ printf "\033[33m%s\033[0m\n" "$*"; }
red()   { printf "\033[31m%s\033[0m\n" "$*"; }

# 注册一条 business_type 到 accounting.account_business_type_info。
# 幂等:同 code 已存在时 accounting 返回 400 "duplicate" 或 409,这里都当 OK 跳过。
# 入参:CODE:account_type[:business_type[:description]]
register_biz_type() {
  local spec="$1"
  local IFS=':'
  # shellcheck disable=SC2206
  local parts=( $spec )
  local code="${parts[0]:-}"
  local atype="${parts[1]:-}"
  local biz="${parts[2]:-0}"           # 0 = 让服务端自动分配 [101, 999]
  local desc="${parts[3]:-loadtest-registered}"
  if [[ -z "${code}" || -z "${atype}" ]]; then
    red "  ✗ --register-biz 格式错误: '${spec}' (期望 CODE:account_type[:biz[:desc]])"
    return 1
  fi

  yellow "[BIZ] ${code} (account_type=${atype} biz=${biz})"
  local payload
  payload=$(cat <<EOF
{
  "account_type": ${atype},
  "business_type_code": "${code}",
  "description": "${desc}",
  "business_type": ${biz}
}
EOF
)
  : > /tmp/biz_resp
  local rc
  rc=$(curl -s -o /tmp/biz_resp -w "%{http_code}" -X POST \
    "${ADMIN_HTTP}/admin/business-types" \
    -H 'Content-Type: application/json' \
    -d "${payload}")
  case "${rc}" in
    200|201) green "  ✓ registered: $(cat /tmp/biz_resp 2>/dev/null)" ;;
    409)     yellow "  • already exists, skip" ;;
    400)
      # accounting 返回 400 + duplicate 提示也当 OK(uk_business_type_code 撞了)
      if grep -qiE "duplicate|already.*exist|exists" /tmp/biz_resp 2>/dev/null; then
        yellow "  • already exists (400 dup), skip"
      else
        red "  ✗ register biz failed (HTTP 400): $(cat /tmp/biz_resp 2>/dev/null)"
        return 1
      fi ;;
    000)
      red "  ✗ connection refused (admin HTTP ${ADMIN_HTTP} unreachable)"
      return 1 ;;
    *)
      red "  ✗ register biz failed (HTTP ${rc}): $(cat /tmp/biz_resp 2>/dev/null)"
      return 1 ;;
  esac
}

# 注册 + provision + switch 单个 LA。幂等：已存在的 LA register 返回 409，
# 这里把 409 视为 OK 继续走 provision + switch（manual-provision 会建下一期
# fleet，已 active 时这会建第二代）。
register_and_provision() {
  local key="$1"
  local account_type="$2"
  local business_type="$3"

  echo
  yellow "[LA] ${key} (type=${account_type} biz=${business_type})"

  # ── 1. register（已存在 → 409 → 跳过）
  local register_payload
  register_payload=$(cat <<EOF
{
  "logical_account_key": "${key}",
  "account_type": ${account_type},
  "account_business_type": ${business_type},
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
if [[ ${#BIZ_TYPES_TO_REGISTER[@]} -gt 0 ]]; then
  green "  extra biz to register: ${BIZ_TYPES_TO_REGISTER[*]}"
fi
green "=========================================================="

# ─── Step 0: 注册额外的 business_type ─────────────────────────────────────
# 跑 LA 注册前先把所有 --register-biz 指定的 biz 塞到 account_business_type_info,
# 否则后面 LA register / ForceProvision 用了未登记的 biz 会被 createAccountInternal
# 的 registry 校验拒掉。预置 biz=1..9 走 init.sql,这里只处理用户扩展的。
if [[ ${#BIZ_TYPES_TO_REGISTER[@]} -gt 0 ]]; then
  green ""
  green "─── Step 0: 注册 ${#BIZ_TYPES_TO_REGISTER[@]} 个额外 business_type ───"
  for spec in "${BIZ_TYPES_TO_REGISTER[@]}"; do
    register_biz_type "${spec}" || red "  (继续往下走;后续用到该 biz 的 LA 会失败)"
  done
fi

IFS=',' read -ra CHAN_ARR <<< "${CHANNELS}"

# 渠道维度：每个渠道 × 4 prefix = 4 个 LA。biz_type 跟 account_type 1:1 对齐(strict)。
for chan in "${CHAN_ARR[@]}"; do
  chan="${chan// /}"
  [[ -z "${chan}" ]] && continue
  for p in "${CHANNEL_PREFIXES[@]}"; do
    key="${p}${chan}"
    register_and_provision "${key}" \
      "$(account_type_for_prefix "${p}")" \
      "$(business_type_for_prefix "${p}")" || true
  done
done

# 平台/通用：每个 prefix 一个 LA（用 "default" 后缀，可按需扩展）。biz_type 同上。
for p in "${PLATFORM_PREFIXES[@]}"; do
  key="${p}default"
  register_and_provision "${key}" \
    "$(account_type_for_prefix "${p}")" \
    "$(business_type_for_prefix "${p}")" || true
done

green ""
green "✅ Fleet 准备完成。下一步："
green "   1. 确保 split-payment SPLIT_PAYMENT_FLEET_ROUTING_ENABLED=true 已经设置（deploy/overrides/split-payment.yml 已默认开）"
green "   2. 跑常规 loadtest：./scripts/run.sh"
green "   3. 跑完看 sub 分布：./scripts/fleet-verify.sh"
