#!/usr/bin/env bash
# ============================================================================
# 一键起栈 + 跑压测
#
# 用法：
#   ./scripts/run.sh                           # 只起栈，不压测
#   ./scripts/run.sh --loadtest                # 起栈 + 60s mixed 压测
#   ./scripts/run.sh --loadtest --mode=topup --concurrency=20 --duration=120s
#   ./scripts/run.sh --loadtest --chaos=restart-accounting --duration=120s
#
# 启动顺序（必须按这个顺序，互相依赖）：
#   1. accounting-system 栈（10 mysql + redis + kafka + accounting-service）
#      ← 用它自己的 docker-compose.yml 起，独立维护
#   2. loadtest 栈（etcd + split-payment + loadtest）
#      ← 本目录的 docker-compose.yml
#
# 前置：
#   docker network create payment-stack       （只跑一次）
#   export GITHUB_TOKEN=ghp_xxx                （build 镜像需要）
# ============================================================================

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)"
MONOREPO_ROOT="$(cd "${DIR}/../../../.." && pwd)"
ACCOUNTING_DIR="${MONOREPO_ROOT}/packages/accounting-system"
cd "${DIR}"

# ─── 解析参数 ─────────────────────────────────────────────────────────────
DO_LOADTEST=0
LT_MODE=""
LT_CONC=""
LT_DUR=""
CHAOS=""
SKIP_ACCOUNTING=0    # 已经手动起好了 accounting 栈
for arg in "$@"; do
  case "$arg" in
    --loadtest) DO_LOADTEST=1 ;;
    --mode=*) LT_MODE="${arg#*=}" ;;
    --concurrency=*) LT_CONC="${arg#*=}" ;;
    --duration=*) LT_DUR="${arg#*=}" ;;
    --chaos=*) CHAOS="${arg#*=}" ;;
    --skip-accounting) SKIP_ACCOUNTING=1 ;;
    -h|--help)
      sed -n '3,22p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
  esac
done

# ─── 前置检查 ─────────────────────────────────────────────────────────────
if ! docker network inspect "${SHARED_DB_NETWORK:-payment-stack}" >/dev/null 2>&1; then
  echo "ERROR: docker network '${SHARED_DB_NETWORK:-payment-stack}' 不存在。先运行：" >&2
  echo "  docker network create ${SHARED_DB_NETWORK:-payment-stack}" >&2
  exit 1
fi

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  echo "WARN: GITHUB_TOKEN 未设置；私仓依赖拉取可能失败" >&2
fi

# ─── Step 1: 起 accounting-system 栈（10 库 + redis + kafka + accounting-service）─
if [[ "${SKIP_ACCOUNTING}" -eq 1 ]]; then
  echo ">>> 跳过 accounting-system 栈启动（--skip-accounting）"
else
  if [[ ! -f "${ACCOUNTING_DIR}/docker-compose.yml" ]]; then
    echo "ERROR: 找不到 ${ACCOUNTING_DIR}/docker-compose.yml" >&2
    exit 1
  fi
  echo ">>> Step 1/2: 起 accounting-system 栈（10 mysql + redis + kafka + accounting-service）"
  echo "    用 loadtest overlay 把 Redis Sentinel 模式覆盖成 single 模式"
  # LOADTEST_DIR 给 overlay yaml 用绝对路径解析 volume mount
  export LOADTEST_DIR="${DIR}"
  ( cd "${ACCOUNTING_DIR}" && \
    docker compose \
      -f docker-compose.yml \
      -f "${DIR}/compose.accounting-override.yml" \
      up -d --build )

  echo ">>> 等 accounting-service 健康（最多 5 分钟，首次建 100 张分表很慢）..."
  for i in $(seq 1 300); do
    if curl -sf http://localhost:8888/admin/health >/dev/null 2>&1; then
      echo "    accounting-service OK (${i}s)"
      break
    fi
    if (( i % 15 == 0 )); then
      echo "    ...等待中 (${i}s)"
    fi
    sleep 1
  done
  if ! curl -sf http://localhost:8888/admin/health >/dev/null 2>&1; then
    echo "ERROR: accounting-service 5 分钟未健康，看日志：" >&2
    echo "  ( cd ${ACCOUNTING_DIR} && docker compose logs accounting-service | tail -50 )" >&2
    exit 1
  fi
fi

# ─── Step 2: 起 loadtest 栈（etcd + split-payment + loadtest）────────────
echo ">>> Step 2/2: 起 loadtest 栈（etcd + split-payment + loadtest）"
docker compose up -d --build etcd split-payment

echo ">>> 等 split-payment 健康..."
for i in $(seq 1 90); do
  if docker compose exec -T split-payment /bin/sh -c 'wget -q -O- http://localhost:9099/admin/health 2>/dev/null' >/dev/null 2>&1; then
    echo "    split-payment OK (${i}s)"
    break
  fi
  sleep 1
done

# ─── Bootstrap LA + rotation policy ──────────────────────────────────────
echo ">>> 预注册 logical_accounts + rotation policy ..."
ACCOUNTING_ADMIN="http://localhost:8888" \
  bash "${DIR}/scripts/bootstrap-logical-accounts.sh" || {
  echo "WARN: bootstrap 失败（可能已经注册过，继续）"
}

# ─── Chaos 注入（可选）─────────────────────────────────────────────────
if [[ -n "${CHAOS}" ]]; then
  case "${CHAOS}" in
    restart-accounting)
      echo ">>> CHAOS: 60s 后重启 accounting-service"
      ( sleep 60 && cd "${ACCOUNTING_DIR}" && \
        docker compose -f docker-compose.yml -f "${DIR}/compose.accounting-override.yml" restart accounting-service ) &
      ;;
    mysql-restart)
      echo ">>> CHAOS: 60s 后 30s 断 mysql-0"
      ( sleep 60 && cd "${ACCOUNTING_DIR}" && \
        docker compose -f docker-compose.yml -f "${DIR}/compose.accounting-override.yml" stop mysql-0 && \
        sleep 30 && cd "${ACCOUNTING_DIR}" && \
        docker compose -f docker-compose.yml -f "${DIR}/compose.accounting-override.yml" start mysql-0 ) &
      ;;
    *)
      echo "WARN: 未知 chaos 模式 '${CHAOS}'，忽略"
      ;;
  esac
fi

# ─── 跑压测 ──────────────────────────────────────────────────────────────
if [[ "${DO_LOADTEST}" -eq 1 ]]; then
  echo ">>> 启动压测..."
  EXTRA=()
  [[ -n "${LT_MODE}" ]] && EXTRA+=("--mode=${LT_MODE}")
  [[ -n "${LT_CONC}" ]] && EXTRA+=("--concurrency=${LT_CONC}")
  [[ -n "${LT_DUR}"  ]] && EXTRA+=("--duration=${LT_DUR}")
  mkdir -p "${DIR}/output"
  docker compose run --rm loadtest \
    /usr/local/bin/loadtest \
    --config=/loadtest/config.yaml \
    "${EXTRA[@]}"
  echo ">>> 压测完毕，报告：${DIR}/output/loadtest-report.json"
else
  echo ">>> 栈已起，未跑压测。"
  echo "    跑压测：./scripts/run.sh --loadtest"
  echo "    清理:   ./scripts/teardown.sh"
fi
