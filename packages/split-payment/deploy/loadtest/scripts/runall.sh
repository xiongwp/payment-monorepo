#!/usr/bin/env bash
# ============================================================================
# Plan A — 真实端到端 booking 压测，一键执行
#
# 流程：
#   1. 前置检查（docker network、GITHUB_TOKEN、accounting 栈在跑）
#   2. 起 etcd + split-payment（账户启动期 reconcile rule 到 accounting）
#   3. 跑 loadtest-bootstrap，预创建 ~703 个账户 → /output/account_pool.json
#   4. 跑 loadtest，dispatch 用真实 account_no 走完整 split-payment → accounting
#   5. 跑完查 mysql 的 transaction_order 行数 + 报错分布
#
# 用法：
#   ./scripts/runall.sh                         # 默认：60s mixed
#   ./scripts/runall.sh --duration=30s          # 缩到 30s
#   ./scripts/runall.sh --concurrency=20        # 并发 20
#   ./scripts/runall.sh --fresh                 # 强制重新 bootstrap pool
#   ./scripts/runall.sh --skip-verify           # 不查 mysql
#
# 前置：
#   docker network create payment-stack         （只跑一次）
#   export GITHUB_TOKEN=ghp_xxx                  （build 镜像需要）
#   ( cd packages/accounting-system && docker compose up -d --build )
# ============================================================================

set -euo pipefail

# ─── 路径定位 ─────────────────────────────────────────────────────────────
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)"
MONOREPO_ROOT="$(cd "${DIR}/../../../.." && pwd)"
ACCOUNTING_DIR="${MONOREPO_ROOT}/packages/accounting-system"
LOG_FILE="/tmp/loadtest-runall-$(date +%Y%m%d-%H%M%S).log"
POOL_FILE="${DIR}/output/account_pool.json"

cd "${DIR}"

# ─── 参数解析 ─────────────────────────────────────────────────────────────
DURATION=""
CONCURRENCY=""
FRESH=0
SKIP_VERIFY=0
for arg in "$@"; do
  case "$arg" in
    --duration=*) DURATION="${arg#*=}" ;;
    --concurrency=*) CONCURRENCY="${arg#*=}" ;;
    --fresh) FRESH=1 ;;
    --skip-verify) SKIP_VERIFY=1 ;;
    -h|--help)
      sed -n '3,28p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *) echo "WARN: 未知参数 '$arg'，忽略" ;;
  esac
done

# ─── 着色 helper ──────────────────────────────────────────────────────────
red()    { printf "\033[31m%s\033[0m\n" "$*"; }
green()  { printf "\033[32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
blue()   { printf "\033[34m%s\033[0m\n" "$*"; }
hr()     { printf "%.0s─" {1..70}; echo; }

step() {
  hr
  blue "▶ $*"
  hr
}

# ─── Step 0: 前置检查 ────────────────────────────────────────────────────
step "Step 0 / 5  前置检查"

if ! docker network inspect "${SHARED_DB_NETWORK:-payment-stack}" >/dev/null 2>&1; then
  red "ERROR: docker network 'payment-stack' 不存在"
  echo "  先跑：docker network create payment-stack"
  exit 1
fi
green "  ✓ payment-stack 网络存在"

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  yellow "  ⚠ GITHUB_TOKEN 未设置 — 私仓 build 可能失败"
else
  green "  ✓ GITHUB_TOKEN 已设置"
fi

if ! docker ps --format '{{.Names}}' | grep -q accounting-service; then
  red "ERROR: accounting-service 容器没在跑"
  echo "  先起 accounting 栈："
  echo "    ( cd ${ACCOUNTING_DIR} && docker compose up -d --build )"
  exit 1
fi
ACCT_CTR=$(docker ps --format '{{.Names}}' | grep accounting-service | head -1)
green "  ✓ accounting-service: ${ACCT_CTR}"

# ─── Step 1: 起 etcd + split-payment ─────────────────────────────────────
step "Step 1 / 5  起 etcd + split-payment"

echo ">>> etcd"
docker compose up -d etcd >>"${LOG_FILE}" 2>&1
for i in $(seq 1 30); do
  if docker compose exec -T etcd /bin/sh -c 'etcdctl endpoint health' >/dev/null 2>&1; then
    green "  ✓ etcd OK (${i}s)"
    break
  fi
  sleep 1
done

echo ">>> split-payment (build + start)"
docker compose up -d --build split-payment >>"${LOG_FILE}" 2>&1
SP_OK=0
for i in $(seq 1 90); do
  if curl -sf http://localhost:19099/healthz >/dev/null 2>&1; then
    green "  ✓ split-payment OK (${i}s)"
    SP_OK=1
    break
  fi
  sleep 1
done
if [[ ${SP_OK} -ne 1 ]]; then
  red "ERROR: split-payment 90s 没起来"
  echo "  日志：docker logs loadtest-split-payment --tail 80"
  exit 1
fi

# 确认 rule reconcile 把 graph 推到 accounting 了
RULES=$(docker logs loadtest-split-payment 2>&1 | grep -oE 'total_rules_upserted":[0-9]+' | tail -1 | grep -oE '[0-9]+' || echo "0")
if [[ "${RULES}" -gt 0 ]]; then
  green "  ✓ rule reconcile: ${RULES} 条 rule 推到 accounting"
else
  yellow "  ⚠ rule reconcile 没看到 → loadtest 可能报 'no transaction rule found'"
fi

# ─── Step 2: build + bootstrap 账户池 ────────────────────────────────────
step "Step 2 / 5  build loadtest + bootstrap 账户池"

echo ">>> 重建 loadtest 镜像（含 loadtest + loadtest-bootstrap）"
docker compose build loadtest >>"${LOG_FILE}" 2>&1
green "  ✓ 镜像 ready"

if [[ ${FRESH} -eq 1 ]] && [[ -f "${POOL_FILE}" ]]; then
  rm -f "${POOL_FILE}"
  yellow "  ⚠ --fresh: 清掉旧 pool"
fi

if [[ -s "${POOL_FILE}" ]]; then
  POOL_USERS=$(grep -oE '"users":\s*\[' "${POOL_FILE}" >/dev/null && \
               python3 -c "import json; d=json.load(open('${POOL_FILE}')); print(len(d.get('users',[])))" 2>/dev/null || echo "?")
  green "  ✓ pool 已存在 (${POOL_FILE}, users=${POOL_USERS})，跳过 bootstrap（用 --fresh 强制重建）"
else
  mkdir -p "${DIR}/output"
  echo ">>> 跑 loadtest-bootstrap..."
  docker compose run --rm --entrypoint /usr/local/bin/loadtest-bootstrap loadtest \
    --accounting-grpc=accounting-service:50051 \
    --accounting-http=http://accounting-service:8888 \
    --output=/output/account_pool.json \
    --num-users=100 --num-merchants=100 --num-channels=100 \
    --currency=PHP --workers=16 2>&1 | tee -a "${LOG_FILE}"
  if [[ ! -s "${POOL_FILE}" ]]; then
    red "ERROR: bootstrap 跑完但 pool 文件没产出"
    exit 1
  fi
  green "  ✓ pool 写出：${POOL_FILE}"
fi

# ─── Step 2.5: 预充值（mysql UPDATE balance=1e15）────────────────────────
# accounting 没暴露 HTTP adjust，所以直接 mysql UPDATE。不充值 → TCC try 全报
# "insufficient available balance"（debit 校验可用余额是 TCC 设计的核心约束）。
# 幂等：UPDATE ... WHERE balance=0 只动初始账户，重跑不影响有交易的账户。
step "Step 2.5 / 5  预充值（mysql UPDATE 把所有 balance=0 账户撑到 1e15）"
bash "${DIR}/scripts/prefund.sh" 2>&1 | tee -a "${LOG_FILE}" || \
  yellow "  ⚠ prefund 部分失败，loadtest 可能仍报 insufficient balance"

# ─── Step 3: 跑 loadtest ─────────────────────────────────────────────────
step "Step 3 / 5  跑压测"

EXTRA=()
[[ -n "${DURATION}" ]] && EXTRA+=("--duration=${DURATION}")
[[ -n "${CONCURRENCY}" ]] && EXTRA+=("--concurrency=${CONCURRENCY}")

echo ">>> 跑 loadtest，输出实时打屏 + 落盘 ${LOG_FILE}"
# `${EXTRA[@]:-}` 形式：数组空时安全展开成空（绕开 set -u "unbound variable" 报错）
docker compose run --rm loadtest \
  /usr/local/bin/loadtest \
  --config=/loadtest/config.yaml \
  ${EXTRA[@]+"${EXTRA[@]}"} 2>&1 | tee -a "${LOG_FILE}"

# ─── Step 4: 验证 mysql 真有 booking 行 ──────────────────────────────────
if [[ ${SKIP_VERIFY} -eq 1 ]]; then
  step "Step 4 / 5  跳过 mysql 验证（--skip-verify）"
else
  step "Step 4 / 5  验证 mysql 落账"

  TOTAL_ORDERS=0
  MYSQL_PWD="${MYSQL_PWD:-password}"
  echo ">>> transaction_order 行数（10 个 shard，每个 shard 100 张 transaction_order_NN 子表）"
  for i in $(seq 0 9); do
    SHARD_CTR=$(docker ps --format '{{.Names}}' | grep -E "accounting.*mysql-${i}\b|mysql-${i}-1" | head -1)
    if [[ -z "${SHARD_CTR}" ]]; then
      yellow "  shard-${i}: 容器找不到"
      continue
    fi
    # 用 information_schema 把所有 transaction_order_NN 子表 sum 起来
    CNT=$(docker exec "${SHARD_CTR}" mysql -uroot -p"${MYSQL_PWD}" -N -se "
      SELECT COALESCE(SUM(c), 0) FROM (
        SELECT (
          SELECT COUNT(*) FROM information_schema.tables t2
          WHERE t2.table_schema=t.table_schema AND t2.table_name=t.table_name
        ) c
        FROM information_schema.tables t
        WHERE t.table_schema LIKE 'accounting_db_%'
          AND t.table_name REGEXP '^transaction_order_[0-9]+\$'
      ) x;
    " 2>/dev/null || echo 0)
    # 上面那个 trick 只是数 table 个数，实际行数要 SUM(COUNT(*)) 每张表。改用动态 SQL：
    CNT=$(docker exec "${SHARD_CTR}" sh -c "
      mysql -uroot -p${MYSQL_PWD} -N -se \"
        SELECT GROUP_CONCAT(CONCAT('SELECT COUNT(*) FROM \`', table_schema, '\`.\`', table_name, '\`') SEPARATOR ' UNION ALL ')
        FROM information_schema.tables
        WHERE table_schema LIKE 'accounting_db_%'
          AND table_name REGEXP '^transaction_order_[0-9]+\$';
      \" 2>/dev/null | head -c 100000 > /tmp/q.sql
      if [[ -s /tmp/q.sql ]]; then
        echo \"SELECT SUM(c) FROM (\$(cat /tmp/q.sql)) x(c);\" | mysql -uroot -p${MYSQL_PWD} -N 2>/dev/null
      else
        echo 0
      fi
    " 2>/dev/null | tail -1)
    CNT=${CNT:-0}
    printf "  shard-%d (%s): %s rows (across all transaction_order_NN)\n" "${i}" "${SHARD_CTR}" "${CNT}"
    TOTAL_ORDERS=$((TOTAL_ORDERS + CNT))
  done
  hr
  if [[ ${TOTAL_ORDERS} -gt 0 ]]; then
    green "✓ 真实落账 ${TOTAL_ORDERS} 条 transaction_order"
  else
    red "✗ 没有 transaction_order 落库 — bookings 没真打进去"
  fi
fi

# ─── Step 5: 汇总 split-payment 错误分布 ──────────────────────────────────
step "Step 5 / 5  split-payment 错误分布（前 10）"

docker logs loadtest-split-payment 2>&1 \
  | grep -oE '"error":"[^"]+"' \
  | sort | uniq -c | sort -rn | head -10 || echo "  (无错误日志)"

hr
green "完成。完整日志：${LOG_FILE}"
echo "  pool 文件：${POOL_FILE}"
echo "  报告 JSON：${DIR}/output/loadtest-report.json"
hr
