#!/usr/bin/env bash
# 清理压测栈 + volume
#
# 用法：
#   ./scripts/teardown.sh           # 停容器，保留 mysql volume
#   ./scripts/teardown.sh --purge   # 停容器 + 删 mysql volume（彻底重置 DB）

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)"
cd "${DIR}"

PURGE=0
for a in "$@"; do
  case "$a" in
    --purge) PURGE=1 ;;
  esac
done

if [[ "${PURGE}" -eq 1 ]]; then
  echo ">>> docker compose down -v  (清空 mysql/redis/etcd 数据)"
  docker compose down -v --remove-orphans
else
  echo ">>> docker compose down  (保留 volume)"
  docker compose down --remove-orphans
fi
echo ">>> 清理完毕"
