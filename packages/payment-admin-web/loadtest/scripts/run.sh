#!/usr/bin/env bash
# ============================================================================
# run.sh —— 一键端到端压测（payment-admin-web 全栈版）
#
# 流程：
#   0. 前置检查 —— payment-stack 网络、关键容器在跑、GITHUB_TOKEN
#   1. build loadtest 镜像
#   2. bootstrap：通过 accounting API 预创建 ~7003 个账户 → output/account_pool.json
#   3. prefund：UPDATE 所有 balance=0 的账户充满 1e15
#   4. loadtest：通过 split-payment.TriggerEvent 跑混合场景 60s
#   5. verify：查 accounting transaction_order 行数 + split-payment 错误分布
#
# 用法：
#   ./scripts/run.sh                          # 默认：mixed × 60s × concurrency=100
#   ./scripts/run.sh --duration=30s           # 缩短
#   ./scripts/run.sh --concurrency=200        # 加大并发
#   ./scripts/run.sh --mode=transfer          # 单跑 transfer（1 leg baseline）
#   ./scripts/run.sh --fresh                  # 清掉 account_pool 强制重 bootstrap
#   ./scripts/run.sh --skip-verify            # 不查 mysql
#
# 前置（只跑一次）：
#   docker network create payment-stack
#   export GITHUB_TOKEN=ghp_xxx
#   cd ../stack && bash deploy.sh up           # 起全栈
# ============================================================================

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)"
LOG_FILE="/tmp/payment-loadtest-$(date +%Y%m%d-%H%M%S).log"
POOL_FILE="${DIR}/output/account_pool.json"
COMPOSE="docker compose -f ${DIR}/docker-compose.loadtest.yml"

cd "${DIR}"
mkdir -p output

# ─── 参数解析 ─────────────────────────────────────────────────────────────
DURATION=""
CONCURRENCY=""
MODE=""
FRESH=0
SKIP_VERIFY=0
for arg in "$@"; do
  case "$arg" in
    --duration=*)    DURATION="${arg#*=}" ;;
    --concurrency=*) CONCURRENCY="${arg#*=}" ;;
    --mode=*)        MODE="${arg#*=}" ;;
    --fresh)         FRESH=1 ;;
    --skip-verify)   SKIP_VERIFY=1 ;;
    -h|--help)
      sed -n '3,30p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *) echo "WARN: 未知参数 '$arg'，忽略" ;;
  esac
done

# ─── 着色 ─────────────────────────────────────────────────────────────────
red()    { printf "\033[31m%s\033[0m\n" "$*"; }
green()  { printf "\033[32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
blue()   { printf "\033[34m%s\033[0m\n" "$*"; }
hr()     { printf "%.0s─" {1..70}; echo; }
step()   { hr; blue "▶ $*"; hr; }

# ─── Step 0: 前置检查 ────────────────────────────────────────────────────
step "Step 0 / 5  前置检查"

if ! docker network inspect payment-stack >/dev/null 2>&1; then
  red "ERROR: docker network 'payment-stack' 不存在"
  echo "  先跑：docker network create payment-stack"
  exit 1
fi
green "  ✓ payment-stack 网络存在"

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  yellow "  ⚠ GITHUB_TOKEN 未设置 — loadtest 镜像 build 可能失败"
else
  green "  ✓ GITHUB_TOKEN 已设置"
fi

# 这些容器必须都在跑（来自 payment-admin-web/stack）
REQUIRED=( accounting-service split-payment redis-sentinel-1 shared-shard-0 )
MISSING=()
for c in "${REQUIRED[@]}"; do
  # 容器名可能带 stack 前缀（compose v2 默认 ${project}-${name}-${idx}），
  # 用模糊匹配兜底
  if ! docker ps --format '{{.Names}}' | grep -qE "(^|-)${c}(-[0-9]+)?\$"; then
    MISSING+=("${c}")
  fi
done
if [[ ${#MISSING[@]} -gt 0 ]]; then
  red "ERROR: 关键容器没在跑：${MISSING[*]}"
  echo "  先起全栈："
  echo "    cd ../stack && bash deploy.sh up"
  exit 1
fi
green "  ✓ payment-admin-web stack 关键容器都在跑"

# ─── Step 1: build loadtest 镜像 ─────────────────────────────────────────
step "Step 1 / 5  build loadtest 镜像（复用 split-payment binary）"

DOCKER_BUILDKIT=1 ${COMPOSE} build loadtest 2>&1 | tee -a "${LOG_FILE}" | tail -8
green "  ✓ 镜像 ready"

# ─── Step 2: bootstrap 账户池 ────────────────────────────────────────────
step "Step 2 / 5  bootstrap 账户池（1000 user / 1000 merchant / 1000 channel）"

if [[ ${FRESH} -eq 1 ]] && [[ -f "${POOL_FILE}" ]]; then
  rm -f "${POOL_FILE}"
  yellow "  ⚠ --fresh: 清掉旧 pool"
fi

if [[ -s "${POOL_FILE}" ]]; then
  POOL_USERS=$(python3 -c "import json; d=json.load(open('${POOL_FILE}')); print(len(d.get('users',[])))" 2>/dev/null || echo "?")
  green "  ✓ pool 已存在 (users=${POOL_USERS})，跳过 bootstrap（用 --fresh 强制重建）"
else
  echo ">>> 跑 loadtest-bootstrap（workers=32, ~7003 账户, 通常 30-90s）..."
  ${COMPOSE} run --rm --entrypoint /usr/local/bin/loadtest-bootstrap loadtest \
    --accounting-grpc=accounting-service:50051 \
    --accounting-http=http://accounting-service:8888 \
    --output=/output/account_pool.json \
    --num-users=1000 --num-merchants=1000 --num-channels=1000 \
    --currency=PHP --workers=32 2>&1 | tee -a "${LOG_FILE}"
  if [[ ! -s "${POOL_FILE}" ]]; then
    red "ERROR: bootstrap 跑完但 pool 文件没产出"
    exit 1
  fi
  green "  ✓ pool 写出：${POOL_FILE}"
fi

# ─── Step 3: 预充值 ──────────────────────────────────────────────────────
step "Step 3 / 5  预充值（balance=0 的账户 UPDATE 到 1e15）"

PREFUND_RC=0
bash "${DIR}/scripts/prefund.sh" 2>&1 | tee -a "${LOG_FILE}" || PREFUND_RC=$?
if [[ ${PREFUND_RC} -ne 0 ]]; then
  red "ERROR: prefund 失败（rc=${PREFUND_RC}） — 不充值跑压测必报 insufficient balance"
  echo "  诊断：bash -x ${DIR}/scripts/prefund.sh 2>&1 | tail -50"
  exit 1
fi

# ─── Step 4: 跑压测 ──────────────────────────────────────────────────────
step "Step 4 / 5  跑端到端压测"

# 拼额外参数（数组写法兼容 set -u，空时安全展开）
EXTRA=()
[[ -n "${DURATION}"    ]] && EXTRA+=("--duration=${DURATION}")
[[ -n "${CONCURRENCY}" ]] && EXTRA+=("--concurrency=${CONCURRENCY}")
[[ -n "${MODE}"        ]] && EXTRA+=("--mode=${MODE}")

echo ">>> 跑 loadtest，输出实时屏 + 落盘 ${LOG_FILE}"
${COMPOSE} run --rm loadtest \
  /usr/local/bin/loadtest \
  --config=/loadtest/config.yaml \
  ${EXTRA[@]+"${EXTRA[@]}"} 2>&1 | tee -a "${LOG_FILE}"

# ─── Step 5: 验证 ────────────────────────────────────────────────────────
if [[ ${SKIP_VERIFY} -eq 1 ]]; then
  step "Step 5 / 5  跳过 mysql 验证（--skip-verify）"
else
  step "Step 5 / 5  验证 + 错误分布"
  bash "${DIR}/scripts/verify.sh" 2>&1 | tee -a "${LOG_FILE}"
fi

hr
green "完成。日志：${LOG_FILE}"
echo "  pool：${POOL_FILE}"
echo "  report：${DIR}/output/loadtest-report.json"
hr
