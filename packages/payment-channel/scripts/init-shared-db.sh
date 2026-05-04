#!/usr/bin/env bash
# One-shot bootstrap for payment-channel on a SHARDED shared MySQL cluster.
#
# Topology (user's existing setup):
#   shared-meta       → paychan_meta
#   shared-shard-N    → paychan_db_N     (N = 0..9)
#
# This script:
#   1. Ensures `payment-stack` docker network exists.
#   2. Connects each shared-shard-N + shared-meta to payment-stack
#      (so our compose services can reach them by DNS).
#   3. Loads each paychan_db_N schema into its matching shared-shard-N,
#      and paychan_meta into shared-meta.
#
# Overrides:
#   SHARED_META_CONTAINER (default shared-meta)
#   SHARED_SHARD_PREFIX  (default shared-shard-)
#   SHARED_DB_NETWORK    (default payment-stack)
#   SHARED_DB_USER/PASS  (default root/password)
#
# Idempotent: CREATE DATABASE IF NOT EXISTS; network connect tolerates
# "already attached".
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
META_SQL="$ROOT/database/metadb/init/init.sql"
META_SHADOW_SQL="$ROOT/database/metadb/init/init_shadow.sql"
SHARD_DIR="$ROOT/database/paychandb/init"

SHARED_DB_NETWORK="${SHARED_DB_NETWORK:-payment-stack}"
SHARED_META_CONTAINER="${SHARED_META_CONTAINER:-shared-meta}"
SHARED_SHARD_PREFIX="${SHARED_SHARD_PREFIX:-shared-shard-}"
SHARED_DB_USER="${SHARED_DB_USER:-root}"
SHARED_DB_PASS="${SHARED_DB_PASS:-password}"

# LOAD_SHADOW=0 跳过影子表导入（生产 / 不跑压测的环境）。默认导入；
# 应用启动期 ApplyShadowTables 也会自愈兜底，导入失败不致命。
LOAD_SHADOW="${LOAD_SHADOW:-1}"

ensure_network() {
  echo "[shared-net] ensure '$SHARED_DB_NETWORK' exists"
  docker network inspect "$SHARED_DB_NETWORK" >/dev/null 2>&1 \
    || docker network create "$SHARED_DB_NETWORK" >/dev/null
}

attach() {
  local container="$1"
  if docker inspect "$container" >/dev/null 2>&1; then
    docker network connect "$SHARED_DB_NETWORK" "$container" 2>/dev/null \
      && echo "[shared-net]   attached $container" \
      || echo "[shared-net]   $container already on $SHARED_DB_NETWORK"
  else
    echo "[shared-net]   SKIP $container (no such container)" >&2
  fi
}

load_sql() {
  local container="$1" file="$2"
  echo "  -> $container : $(basename "$file")"
  docker exec -i "$container" \
    mysql -u"$SHARED_DB_USER" -p"$SHARED_DB_PASS" --default-character-set=utf8mb4 \
    < "$file"
}

ensure_network
attach "$SHARED_META_CONTAINER"
for i in 0 1 2 3 4 5 6 7 8 9; do
  attach "${SHARED_SHARD_PREFIX}${i}"
done

echo "[shared-db] load paychan_meta into $SHARED_META_CONTAINER"
load_sql "$SHARED_META_CONTAINER" "$META_SQL"

echo "[shared-db] load paychan_db_N into matching shared-shard-N"
for i in 0 1 2 3 4 5 6 7 8 9; do
  load_sql "${SHARED_SHARD_PREFIX}${i}" "$SHARD_DIR/${i}_init.sql"
done

if [ "$LOAD_SHADOW" = "1" ]; then
  if [ -f "$META_SHADOW_SQL" ]; then
    echo "[shared-db] load paychan_meta shadow into $SHARED_META_CONTAINER"
    load_sql "$SHARED_META_CONTAINER" "$META_SHADOW_SQL"
  fi
  echo "[shared-db] load paychan_db_N shadow into matching shared-shard-N"
  for i in 0 1 2 3 4 5 6 7 8 9; do
    if [ -f "$SHARD_DIR/${i}_init_shadow.sql" ]; then
      load_sql "${SHARED_SHARD_PREFIX}${i}" "$SHARD_DIR/${i}_init_shadow.sql"
    fi
  done
fi

echo "[shared-db] payment-channel init complete."
