#!/usr/bin/env bash
# sync-from-childrepo.sh — 把 child repo 的提交反向拉回 monorepo 的
# packages/<name>/。仅在「child repo 临时被直接修改」的应急场景使用。
#
# 工作流约定：默认 monorepo 是单一真理源；child repo 应只接受来自
# sync-to-childrepos.sh 的 push。但出于以下原因仍可能临时回流：
#   - 第三方 PR 直接打到 child repo
#   - 紧急 hotfix 在 child 仓修了再补回 monorepo
#   - 历史代码迁移
#
# 用法：
#   scripts/sync-from-childrepo.sh order-core            # 默认 main 分支
#   BRANCH=hotfix scripts/sync-from-childrepo.sh order-core
#
# 内部：git subtree pull --squash --prefix=packages/<name> <name> <branch>

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

BRANCH="${BRANCH:-main}"

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <package-name>" >&2
  echo "       BRANCH=<branch> $0 <package-name>" >&2
  exit 2
fi

pkg="$1"
prefix="packages/${pkg}"

if [[ ! -d "${prefix}" ]]; then
  echo "error: ${prefix} does not exist" >&2
  exit 1
fi
if ! git remote get-url "${pkg}" > /dev/null 2>&1; then
  echo "error: git remote '${pkg}' not configured" >&2
  exit 1
fi

# 校验工作区干净，避免 squash merge 卡在脏文件
if ! git diff-index --quiet HEAD --; then
  echo "error: working tree dirty; commit or stash first" >&2
  exit 1
fi

echo "==> pulling ${pkg}/${BRANCH} -> ${prefix}"
git fetch "${pkg}" "${BRANCH}"
git subtree pull --prefix="${prefix}" "${pkg}" "${BRANCH}" --squash \
  -m "subtree pull from ${pkg}/${BRANCH}"
echo "==> done. review the squashed commit and push to origin/main when ready."
