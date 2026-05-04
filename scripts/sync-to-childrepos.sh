#!/usr/bin/env bash
# sync-to-childrepos.sh — 把 monorepo 里 packages/<name>/ 的提交反馈到对应的
# 独立 child repo（git subtree push 模式）。
#
# 工作流约定（"monorepo 单一真理源"）：
#   - 所有代码修改先合并到 payment-monorepo 的 main
#   - 本脚本把 packages/<name>/ 的改动 squash 推到 <name> 这个 git remote
#   - child repo 不接受外部直接 commit；如果有需要紧急直推 child 再回流，先与
#     team 沟通，回流时 monorepo 一侧用 `git subtree pull --prefix=packages/<name> <name> main --squash`
#
# 用法：
#   scripts/sync-to-childrepos.sh                     # 同步所有有改动的 package
#   scripts/sync-to-childrepos.sh order-core          # 只同步指定 package
#   scripts/sync-to-childrepos.sh --all               # 强制全部 package 推一次
#   BRANCH=develop scripts/sync-to-childrepos.sh ...  # 推到 child repo 的指定分支（默认 main）
#   DRY_RUN=1 scripts/sync-to-childrepos.sh           # 只打印将要执行的命令，不真推
#
# 退出码：0 全成功；非 0 至少一个 package push 失败（仍会尝试其余 package）

set -uo pipefail

cd "$(git rev-parse --show-toplevel)"

BRANCH="${BRANCH:-main}"
DRY_RUN="${DRY_RUN:-0}"

# REMOTE_URL_TEMPLATE：把 push target 的 URL 直接 inline 到 git subtree push 里，
# 绕开"先 git remote add 再用 remote 名字推"模式。
# 占位符 `{pkg}` 会被替换成 package 名字。
#
# CI 用法（GitHub Actions）：
#   REMOTE_URL_TEMPLATE='https://x-access-token:'"${CHILDREPO_PAT}"'@github.com/xiongwp/{pkg}.git'
# 这样每次 push 直接把 PAT 注入到 push URL，避免 git-subtree 内部把 PAT 弄丢、
# 走 fallback auth 被 github-actions[bot] token 劫持（实测症状：fetch 阶段
# PAT 还在，push 阶段被劫持成 bot token，报"denied to github-actions[bot]"）。
#
# 本地用法（dev 机）：留空，脚本会回退到"用 git remote 名字"模式（要求本地
# 已经 git remote add 过对应的 remote）。
REMOTE_URL_TEMPLATE="${REMOTE_URL_TEMPLATE:-}"

# 17 个 child repo（必须与 packages/ 子目录名 + git remote 名一致）。
# 新加 package 时同时往这个数组里加 + 在 git remote 里 add。
ALL_PACKAGES=(
  accounting-admin-web
  accounting-grpc-api
  accounting-system
  api-gateway
  billing-system
  clearing-settlement
  id-generator
  kms-manage
  order-core
  payment-admin-web
  payment-channel
  payment-core
  payment-gateway
  payment-util
  reconplatform
  risk-manage
  user-merchant-core
)

# 解析参数
TARGETS=()
FORCE_ALL=0
case "${1:-}" in
  --all)
    FORCE_ALL=1
    TARGETS=("${ALL_PACKAGES[@]}")
    ;;
  "")
    # 默认：自动检测最近一次 push 后改动的 package
    ;;
  *)
    TARGETS=("$@")
    ;;
esac

# 自动检测变更的 package：对比 origin/$BRANCH..HEAD（如果远端存在）或者最近 1 次提交
detect_changed_packages() {
  local diff_range
  if git rev-parse --verify --quiet "origin/${BRANCH}" > /dev/null; then
    diff_range="origin/${BRANCH}..HEAD"
  else
    diff_range="HEAD~1..HEAD"
  fi
  git diff --name-only "${diff_range}" 2>/dev/null \
    | awk -F/ '/^packages\// { print $2 }' \
    | sort -u
}

if [[ ${FORCE_ALL} -eq 0 && ${#TARGETS[@]} -eq 0 ]]; then
  while IFS= read -r p; do
    [[ -z "$p" ]] && continue
    TARGETS+=("$p")
  done < <(detect_changed_packages)
fi

if [[ ${#TARGETS[@]} -eq 0 ]]; then
  echo "no packages changed since origin/${BRANCH}; nothing to sync." >&2
  exit 0
fi

echo "==> sync targets: ${TARGETS[*]}"
echo "==> branch: ${BRANCH}"
echo "==> dry-run: ${DRY_RUN}"
echo

failures=()
for pkg in "${TARGETS[@]}"; do
  prefix="packages/${pkg}"

  # 校验包目录与 remote 都存在
  if [[ ! -d "${prefix}" ]]; then
    echo "  [SKIP] ${pkg}: directory ${prefix} missing"
    failures+=("${pkg}: missing dir")
    continue
  fi
  # push target 优先用 REMOTE_URL_TEMPLATE（CI 模式：URL 里 inline PAT），
  # 否则回落到"git remote 名字"模式（dev 模式：本地 git remote add 过）。
  if [[ -n "${REMOTE_URL_TEMPLATE}" ]]; then
    push_target="${REMOTE_URL_TEMPLATE//\{pkg\}/${pkg}}"
    push_target_label="${pkg} (inline URL)"   # log 安全：不打 URL，避免 token 泄漏
  elif git remote get-url "${pkg}" > /dev/null 2>&1; then
    push_target="${pkg}"
    push_target_label="${pkg}"
  else
    echo "  [SKIP] ${pkg}: no REMOTE_URL_TEMPLATE set and git remote not configured"
    echo "         (CI: set REMOTE_URL_TEMPLATE; dev: 'git remote add ${pkg} <url>')"
    failures+=("${pkg}: missing remote")
    continue
  fi

  echo "==> pushing ${prefix} -> ${push_target_label}/${BRANCH}"
  cmd=(git subtree push --prefix="${prefix}" "${push_target}" "${BRANCH}")
  if [[ ${DRY_RUN} -eq 1 ]]; then
    echo "    DRY: ${cmd[*]}"
    continue
  fi

  if ! "${cmd[@]}"; then
    echo "  [FAIL] ${pkg}: subtree push failed (see error above)" >&2
    echo "  hint: child repo may have diverged — try \\"
    echo "        git subtree pull --prefix=${prefix} ${pkg} ${BRANCH} --squash" >&2
    failures+=("${pkg}: push failed")
    continue
  fi
  echo "  [OK]   ${pkg}"
  echo
done

if [[ ${#failures[@]} -gt 0 ]]; then
  echo "==> ${#failures[@]} failure(s):" >&2
  printf '    - %s\n' "${failures[@]}" >&2
  exit 1
fi
echo "==> all targets synced cleanly."
