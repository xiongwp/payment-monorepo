#!/usr/bin/env bash
# 清理压测栈（两层）
#
# 用法：
#   ./scripts/teardown.sh                # 停 loadtest 栈（etcd + split-payment + loadtest）
#   ./scripts/teardown.sh --all          # 同时停 accounting-system 栈
#   ./scripts/teardown.sh --all --purge  # 停所有 + 删 mysql/redis volume

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)"
MONOREPO_ROOT="$(cd "${DIR}/../../../.." && pwd)"
ACCOUNTING_DIR="${MONOREPO_ROOT}/packages/accounting-system"
cd "${DIR}"

ALL=0
PURGE=0
for a in "$@"; do
  case "$a" in
    --all) ALL=1 ;;
    --purge) PURGE=1 ;;
  esac
done

DOWN_ARGS=(--remove-orphans)
if [[ "${PURGE}" -eq 1 ]]; then
  DOWN_ARGS+=(-v)
  echo ">>> 模式: 含 volume 清理"
fi

echo ">>> 停 loadtest 栈"
docker compose down "${DOWN_ARGS[@]}"

if [[ "${ALL}" -eq 1 ]]; then
  echo ">>> 停 accounting-system 栈"
  ( cd "${ACCOUNTING_DIR}" && docker compose down "${DOWN_ARGS[@]}" )
fi

echo ">>> 清理完毕"
