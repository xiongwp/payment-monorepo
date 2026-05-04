#!/usr/bin/env bash
# 停全部 7 个工程 + shared MySQL + payment-stack 网络。
#
# 默认 `down`（保留数据卷）。传 `--wipe` 连数据一起清。
#
# 用法：
#   ./stop-all.sh
#   ./stop-all.sh --wipe

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ "$(basename "$SCRIPT_DIR")" == "scripts" ]]; then
  ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
else
  ROOT="$SCRIPT_DIR"
fi
NETWORK="${SHARED_DB_NETWORK:-payment-stack}"
WIPE=0
[[ "${1:-}" == "--wipe" ]] && WIPE=1

log() { printf '\033[36m[stop-all]\033[0m %s\n' "$*"; }

COMPOSE_DIRS=(
  payment-admin-web
  payment-core
  payment-channel
  user-merchant-core
  order-core
  risk-manage
  kms-manage
)

compose_down() {
  local dir="$1"
  log "→ $dir: docker compose down$([[ $WIPE -eq 1 ]] && echo ' --volumes')"
  if [[ $WIPE -eq 1 ]]; then
    (cd "$ROOT/$dir" && docker compose down --volumes --remove-orphans) || true
  else
    (cd "$ROOT/$dir" && docker compose down --remove-orphans) || true
  fi
}

for d in "${COMPOSE_DIRS[@]}"; do
  compose_down "$d"
done

# shared-meta + shared-shard-0..9 是本脚本 docker run 起来的，单独清
SHARED=(shared-meta shared-shard-0 shared-shard-1 shared-shard-2 shared-shard-3 shared-shard-4 shared-shard-5 shared-shard-6 shared-shard-7 shared-shard-8 shared-shard-9)
for c in "${SHARED[@]}"; do
  if docker inspect "$c" >/dev/null 2>&1; then
    log "rm $c"
    if [[ $WIPE -eq 1 ]]; then
      docker rm -f -v "$c" >/dev/null
    else
      docker rm -f "$c" >/dev/null
    fi
  fi
done

if docker network inspect "$NETWORK" >/dev/null 2>&1; then
  log "remove network $NETWORK"
  docker network rm "$NETWORK" >/dev/null 2>&1 || log "  (skip: network still in use)"
fi

log "✓ stopped."
