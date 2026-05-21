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
  # ─── Step 0: 先把 etcd 起来 ───────────────────────────────────────────
  # accounting-service 启动时会向 etcd:2379 grant lease 注册自己（5s 超时）。
  # 如果先起 accounting 栈、再起 loadtest 栈（含 etcd），accounting 启动期
  # etcd DNS 解析失败 → gRPC server exit。所以 etcd 必须最早起。
  echo ">>> Step 0/3: 先把 etcd 起来（accounting-service 注册要用）"
  docker compose up -d etcd
  for i in $(seq 1 30); do
    if docker compose exec -T etcd /bin/sh -c 'etcdctl endpoint health' >/dev/null 2>&1; then
      echo "    etcd OK (${i}s)"
      break
    fi
    sleep 1
  done

  if [[ ! -f "${ACCOUNTING_DIR}/docker-compose.yml" ]]; then
    echo "ERROR: 找不到 ${ACCOUNTING_DIR}/docker-compose.yml" >&2
    exit 1
  fi
  echo ">>> Step 1/3: 起 accounting-system 栈（10 mysql + redis + kafka + accounting-service）"
  echo "    用 loadtest overlay 把 Redis Sentinel 模式覆盖成 single 模式"
  # LOADTEST_DIR 给 overlay yaml 用绝对路径解析 volume mount
  export LOADTEST_DIR="${DIR}"

  # 关键：`docker compose up -d` 可能因为 accounting-batchtask 的
  # depends_on: accounting-service condition: service_healthy 在 mysql init 阶段
  # 短暂 unhealthy 而返回非零（10 个 shard 各自建 100 张表，首次 1-2 min）。
  # 但 accounting-service 自己其实是好的——所以我们容忍 compose up 的失败，
  # 转而自己轮询 /admin/health 决定是否继续。
  set +e
  ( cd "${ACCOUNTING_DIR}" && \
    docker compose \
      -f docker-compose.yml \
      -f "${DIR}/compose.accounting-override.yml" \
      up -d --build )
  COMPOSE_RC=$?
  set -e
  if [[ ${COMPOSE_RC} -ne 0 ]]; then
    echo "    NOTE: compose up 退出码 ${COMPOSE_RC}（可能 batchtask 等 service_healthy 超时），继续轮询 /admin/health"
  fi

  # NOTE: 不再硬卡死等 accounting-service /admin/health。
  # 原因：
  #   1) accounting compose 给的是端口范围映射（8888-8898:8888），host 侧端口经常
  #      不是 8888，curl localhost:8888 在 macOS Docker Desktop 上会假阴性。
  #   2) docker HEALTHCHECK 本身有滞后（首次 healthy 之前会显示 unhealthy），
  #      但服务实际已经在监听 50051 / 8888 内部端口，loadtest 走容器网络访问
  #      不受 host 端口映射影响。
  # 这里只做一次 best-effort 探测；探不到也继续，让 split-payment / loadtest 自己
  # 通过 docker 内网解析 accounting-service:50051 — 那个路径才是真的服务可达性。
  echo ">>> best-effort 探一下 accounting-service /admin/health（不强制）..."
  if curl -sf -m 2 http://localhost:8888/admin/health >/dev/null 2>&1; then
    echo "    accounting-service host:8888 可达"
  else
    echo "    accounting-service host:8888 探不到（很可能 host port 偏移），跳过；后续走容器内网"
  fi

  # 若刚才 compose up 没把 batchtask 起来（因为它的 depends_on 失败了），
  # 现在 service 健康了，再 up 一次 batchtask 单独把它带起来（幂等）。
  if [[ ${COMPOSE_RC} -ne 0 ]]; then
    echo ">>> 补起可能没起来的 accounting-batchtask"
    ( cd "${ACCOUNTING_DIR}" && \
      docker compose -f docker-compose.yml -f "${DIR}/compose.accounting-override.yml" \
        up -d accounting-batchtask ) || \
      echo "    WARN: batchtask 补起失败（非致命，loadtest 不依赖它）"
  fi
fi

# ─── Step 2: 起 loadtest 栈剩余部分（split-payment + loadtest 镜像构建）─────
echo ">>> Step 2/3: 起 split-payment（etcd 已在 Step 0 起）"
docker compose up -d --build split-payment

echo ">>> 等 split-payment 健康（/healthz）..."
SP_OK=0
for i in $(seq 1 90); do
  # split-payment admin HTTP 默认 :9099，端点是 /healthz（不是 /admin/health）
  if curl -sf http://localhost:19099/healthz >/dev/null 2>&1; then
    echo "    split-payment OK (${i}s)"
    SP_OK=1
    break
  fi
  sleep 1
done
if [[ ${SP_OK} -ne 1 ]]; then
  echo "WARN: split-payment /healthz 90s 内没通；可能 admin port 未暴露或服务正在重试连 accounting。"
  echo "      看日志：docker logs loadtest-split-payment --tail 50"
fi

# ─── Bootstrap LA + rotation policy ──────────────────────────────────────
# 通过 split-payment 容器的网络命名空间打 accounting-service:8888（内网，绕过
# host 的 port range 偏移 / unhealthy false alarm）。-e 把 admin endpoint 注入
# bootstrap 脚本要读的环境变量。
echo ">>> 预注册 logical_accounts + rotation policy（容器内网走 accounting-service:8888）..."
docker compose exec -T \
  -e ACCOUNTING_ADMIN="http://accounting-service:8888" \
  -e SPLIT_PAYMENT_ADMIN="http://split-payment:9099" \
  split-payment bash -c '
    apk add --no-cache curl bash >/dev/null 2>&1 || true
    # 用 split-payment 容器跑 bootstrap：它有 curl，也在 payment-stack 网络里
    cat > /tmp/bootstrap.sh <<'\''EOF'\''
'"$(cat "${DIR}/scripts/bootstrap-logical-accounts.sh")"'
EOF
    bash /tmp/bootstrap.sh
  ' || {
  echo "WARN: bootstrap 失败 — accounting-service 内网仍不可达，或 LA 已注册"
  echo "      手动确认：docker compose -f ${ACCOUNTING_DIR}/docker-compose.yml -f ${DIR}/compose.accounting-override.yml ps"
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
  # 每次都重建 loadtest 镜像 —— `docker compose run` 不带 --build，且没有
  # 用 up 拉起 loadtest（它本来就是按需 run --rm），所以 cmd/loadtest/main.go
  # 或 cmd/loadtest-bootstrap/main.go 的修改不会自动进镜像。这里强制 build。
  echo ">>> 重建 loadtest 镜像（含 loadtest + loadtest-bootstrap）"
  docker compose build loadtest

  # ─── Plan A: 先跑 loadtest-bootstrap 预创建 ~700 个账户 + 写 pool.json ───
  # 跑过一次后 account_pool.json 就常驻 ./output/，下次 rerun 直接复用。
  # 想强制重跑：先 rm output/account_pool.json
  POOL_FILE="${DIR}/output/account_pool.json"
  if [[ -s "${POOL_FILE}" ]]; then
    echo ">>> account_pool.json 已存在 (${POOL_FILE})，跳过 bootstrap"
    echo "    强制重跑：rm ${POOL_FILE}"
  else
    echo ">>> 跑 loadtest-bootstrap 预创建账户池..."
    mkdir -p "${DIR}/output"
    docker compose run --rm --entrypoint /usr/local/bin/loadtest-bootstrap loadtest \
      --accounting-grpc=accounting-service:50051 \
      --accounting-http=http://accounting-service:8888 \
      --output=/output/account_pool.json \
      --num-users=100 --num-merchants=100 --num-channels=100 \
      --currency=PHP --workers=16
    if [[ ! -s "${POOL_FILE}" ]]; then
      echo "ERROR: bootstrap 跑完但没生成 ${POOL_FILE}" >&2
      exit 1
    fi
  fi

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
