#!/usr/bin/env bash
# 一键启动全部 7 个工程的 docker 联栈。
#
# 拓扑（全部挂在 external 网络 payment-stack 上）：
#   kms-manage              9290 gRPC / 9390 metrics
#   risk-manage             9490 gRPC / 9590 metrics
#   order-core              9091 gRPC / 9095 metrics（+ order-meta 3316, order-db-0..9 3306-3315）
#   user-merchant-core      9191 gRPC / 9291 metrics（+ user-merchant-meta 3326）
#   payment-channel         9092 gRPC / 9192 webhook / 9093 metrics / 9400 mockserver
#     └── 依赖 shared-meta + shared-shard-0..9（本脚本会起一套最小 shared 集群）
#   payment-core            9090 gRPC / 9190 metrics
#   payment-admin-web       8190 API / 8080 UI
#
# 用法：
#   ./start-all.sh          # 起全部
#   ./start-all.sh status   # 看状态
#   ./start-all.sh logs     # tail 全部日志
#   ./stop-all.sh           # 停全部（保留数据卷）

set -euo pipefail

# 脚本支持两种放置：
#   - 放在 sibling 根 /home/user/       → ROOT 就是脚本所在目录
#   - 放在 user-merchant-core/scripts/  → ROOT 是上两级
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ "$(basename "$SCRIPT_DIR")" == "scripts" ]]; then
  ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
else
  ROOT="$SCRIPT_DIR"
fi
NETWORK="${SHARED_DB_NETWORK:-payment-stack}"

# ── 共享 MySQL 集群（payment-channel 的 shared-meta + shared-shard-0..9） ────
# payment-channel 硬编码期望这些容器名；单独起一套最小 MySQL 服务（非 compose），
# 方便后续独立清理。
SHARED_META="shared-meta"
SHARED_SHARDS=(shared-shard-0 shared-shard-1 shared-shard-2 shared-shard-3 shared-shard-4 shared-shard-5 shared-shard-6 shared-shard-7 shared-shard-8 shared-shard-9)

# Host 端口从 3406 起，避开 order-core 的 3306-3316。
SHARED_META_HOST_PORT=3406
SHARED_SHARD_HOST_PORT_BASE=3407  # shared-shard-N → 3407 + N

log()  { printf '\033[36m[start-all]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[start-all]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[start-all]\033[0m %s\n' "$*" >&2; exit 1; }

ensure_network() {
  if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
    log "create docker network $NETWORK"
    docker network create "$NETWORK" >/dev/null
  fi
}

ensure_kms_keys() {
  local key_dir="$ROOT/kms-manage/keys"
  if [[ -f "$key_dir/main.key" ]]; then
    log "kms-manage key already bootstrapped"
    return
  fi
  log "bootstrap kms-manage master key into $key_dir"
  mkdir -p "$key_dir"
  docker run --rm \
    -v "$key_dir":/var/lib/kms-manage/keys \
    --entrypoint /usr/local/bin/kmsctl \
    kms-manage:local init-key /var/lib/kms-manage/keys main 2>/dev/null || {
      warn "kms-manage:local image not built yet; will bootstrap after first build."
  }
}

start_shared_mysql() {
  # 每个 shared-* 节点是独立 mysql:8.0 容器。schema 由 payment-channel 的
  # scripts/init-shared-db.sh 在后面统一 load。
  local c p
  c="$SHARED_META"; p="$SHARED_META_HOST_PORT"
  if ! docker inspect "$c" >/dev/null 2>&1; then
    log "start $c (host :$p)"
    docker run -d --name "$c" --network "$NETWORK" --restart unless-stopped \
      -e MYSQL_ROOT_PASSWORD=password \
      -p "$p:3306" \
      mysql:8.0 \
      --character-set-server=utf8mb4 --collation-server=utf8mb4_unicode_ci >/dev/null
  else
    log "$c already running"
    docker network connect "$NETWORK" "$c" 2>/dev/null || true
  fi

  local i
  for i in 0 1 2 3 4 5 6 7 8 9; do
    c="shared-shard-$i"
    p=$((SHARED_SHARD_HOST_PORT_BASE + i))
    if ! docker inspect "$c" >/dev/null 2>&1; then
      log "start $c (host :$p)"
      docker run -d --name "$c" --network "$NETWORK" --restart unless-stopped \
        -e MYSQL_ROOT_PASSWORD=password \
        -p "$p:3306" \
        mysql:8.0 \
        --character-set-server=utf8mb4 --collation-server=utf8mb4_unicode_ci >/dev/null
    else
      log "$c already running"
      docker network connect "$NETWORK" "$c" 2>/dev/null || true
    fi
  done
}

wait_mysql_ready() {
  local c
  for c in "$SHARED_META" "${SHARED_SHARDS[@]}"; do
    log "wait $c ready"
    local i=0
    until docker exec "$c" mysqladmin ping -uroot -ppassword --silent >/dev/null 2>&1; do
      i=$((i+1))
      [[ $i -gt 60 ]] && die "$c not ready after 120s"
      sleep 2
    done
  done
}

compose_up() {
  local name="$1" dir="$2"
  log "→ $name: docker compose up -d --build"
  (cd "$dir" && docker compose up -d --build)
}

cmd_start() {
  ensure_network

  # 1. 基础设施：kms / risk （无下游依赖）
  compose_up "kms-manage"         "$ROOT/kms-manage"
  # 首次 build 完 kms-manage:local 镜像后才能跑 kmsctl 初始化
  ensure_kms_keys
  compose_up "risk-manage"        "$ROOT/risk-manage"

  # 2. order-core + user-merchant-core（各自带 MySQL）
  compose_up "order-core"         "$ROOT/order-core"
  compose_up "user-merchant-core" "$ROOT/user-merchant-core"

  # 3. shared MySQL（给 payment-channel 用）+ schema
  start_shared_mysql
  wait_mysql_ready
  log "load payment-channel schema into shared-meta / shared-shard-*"
  SHARED_DB_NETWORK="$NETWORK" bash "$ROOT/payment-channel/scripts/init-shared-db.sh"

  # 4. payment-channel（需要 shared-*）
  compose_up "payment-channel"    "$ROOT/payment-channel"

  # 5. payment-core（依赖 payment-channel + kms + risk gRPC）
  compose_up "payment-core"       "$ROOT/payment-core"

  # 6. admin BFF + Web（依赖 order-core + payment-core + kms）
  compose_up "payment-admin-web"  "$ROOT/payment-admin-web"

  echo
  log "✓ all stacks up. 关键入口："
  echo "    admin web        http://localhost:8080"
  echo "    admin API        http://localhost:8190"
  echo "    order-core       grpc://localhost:9091   metrics http://localhost:9095/metrics"
  echo "    user-merchant    grpc://localhost:9191   metrics http://localhost:9291/metrics"
  echo "    payment-core     grpc://localhost:9090   metrics http://localhost:9190/metrics"
  echo "    payment-channel  grpc://localhost:9092   webhook http://localhost:9192   metrics http://localhost:9093/metrics"
  echo "    kms-manage       grpc://localhost:9290   metrics http://localhost:9390/metrics"
  echo "    risk-manage      grpc://localhost:9490   metrics http://localhost:9590/metrics"
}

cmd_status() {
  docker ps --filter network="$NETWORK" \
    --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
}

cmd_logs() {
  local containers
  containers="$(docker ps --filter network="$NETWORK" --format '{{.Names}}' | tr '\n' ' ')"
  [[ -z "$containers" ]] && die "no containers on $NETWORK"
  # shellcheck disable=SC2086
  docker logs -f --tail=50 $containers
}

case "${1:-up}" in
  up|start|"")   cmd_start ;;
  status|ps)     cmd_status ;;
  logs)          cmd_logs ;;
  *) echo "usage: $0 [up|status|logs]"; exit 2 ;;
esac
