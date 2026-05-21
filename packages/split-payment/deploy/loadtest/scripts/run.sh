#!/usr/bin/env bash
# ============================================================================
# 一键起栈 + 跑压测
#
# 用法：
#   ./scripts/run.sh                           # 只起栈，不压测
#   ./scripts/run.sh --loadtest                # 起栈 + 60s mixed 压测（默认）
#   ./scripts/run.sh --loadtest --mode=topup --concurrency=20 --duration=120s
#   ./scripts/run.sh --loadtest --chaos=restart-accounting
#
# 前置：
#   1. docker network create payment-stack  （只需一次）
#   2. export GITHUB_TOKEN=ghp_xxx           （build 镜像时透传给 BuildKit）
# ============================================================================

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)"
cd "${DIR}"

# ─── 解析参数 ─────────────────────────────────────────────────────────────
DO_LOADTEST=0
LT_MODE=""
LT_CONC=""
LT_DUR=""
CHAOS=""
for arg in "$@"; do
  case "$arg" in
    --loadtest) DO_LOADTEST=1 ;;
    --mode=*) LT_MODE="${arg#*=}" ;;
    --concurrency=*) LT_CONC="${arg#*=}" ;;
    --duration=*) LT_DUR="${arg#*=}" ;;
    --chaos=*) CHAOS="${arg#*=}" ;;
    -h|--help)
      sed -n '3,15p' "${BASH_SOURCE[0]}"
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

# ─── 启动栈 ───────────────────────────────────────────────────────────────
echo ">>> docker compose up -d  (会构建镜像，首次约 3-5 min)"
docker compose up -d --build

echo ">>> 等待 accounting-system 健康..."
for i in $(seq 1 60); do
  if docker compose exec -T accounting-system /bin/sh -c 'wget -q -O- http://localhost:9092/admin/health 2>/dev/null' >/dev/null 2>&1; then
    echo "    accounting-system OK (${i}s)"
    break
  fi
  sleep 1
done

echo ">>> 等待 split-payment 健康..."
for i in $(seq 1 60); do
  if docker compose exec -T split-payment /bin/sh -c 'wget -q -O- http://localhost:9099/admin/health 2>/dev/null' >/dev/null 2>&1; then
    echo "    split-payment OK (${i}s)"
    break
  fi
  sleep 1
done

# ─── Bootstrap LA + rotation policy ──────────────────────────────────────
echo ">>> 预注册 logical_accounts + rotation policy ..."
bash "${DIR}/scripts/bootstrap-logical-accounts.sh" || {
  echo "WARN: bootstrap 失败（可能已经注册过，继续）"
}

# ─── Chaos 注入（可选）─────────────────────────────────────────────────
if [[ -n "${CHAOS}" ]]; then
  case "${CHAOS}" in
    restart-accounting)
      echo ">>> CHAOS: 60s 后重启 accounting-system"
      ( sleep 60 && docker compose restart accounting-system ) &
      ;;
    mysql-restart)
      echo ">>> CHAOS: 60s 后 30s 断 mysql-0"
      ( sleep 60 && docker compose stop mysql-0 && sleep 30 && docker compose start mysql-0 ) &
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
