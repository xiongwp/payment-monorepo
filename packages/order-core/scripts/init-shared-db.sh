#!/usr/bin/env bash
# One-shot bootstrap for order-core on a SHARDED shared MySQL cluster.
#
# Topology (user's existing setup):
#   shared-meta       → order_meta
#   shared-shard-N    → order_db_N       (N = 0..9)
#
# This script:
#   1. Ensures `payment-stack` docker network exists.
#   2. Connects shared-meta + shared-shard-0..9 to payment-stack.
#   3. Loads each order_db_N into its matching shared-shard-N, and
#      order_meta into shared-meta.
#
# Overrides: same env vars as payment-channel's init-shared-db.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
META_SQL="$ROOT/database/metadb/init/init.sql"
SHARD_DIR="$ROOT/database/orderdb/init"

SHARED_DB_NETWORK="${SHARED_DB_NETWORK:-payment-stack}"
SHARED_META_CONTAINER="${SHARED_META_CONTAINER:-shared-meta}"
SHARED_SHARD_PREFIX="${SHARED_SHARD_PREFIX:-shared-shard-}"
SHARED_DB_USER="${SHARED_DB_USER:-root}"
SHARED_DB_PASS="${SHARED_DB_PASS:-password}"

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

echo "[shared-db] load order_meta into $SHARED_META_CONTAINER"
load_sql "$SHARED_META_CONTAINER" "$META_SQL"

echo "[shared-db] load order_db_N into matching shared-shard-N"
for i in 0 1 2 3 4 5 6 7 8 9; do
  load_sql "${SHARED_SHARD_PREFIX}${i}" "$SHARD_DIR/${i}_init.sql"
done

echo "[shared-db] order-core init complete."
