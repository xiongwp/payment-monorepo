#!/usr/bin/env bash
# verify-build.sh — quick build sweep across the monorepo.
#
# 每个 packages/<svc>/ 子目录里跑 `go build ./...`, 把 fail 项收集后一次性报告.
# 比 docker compose build 快 ~30 倍, 用来 build error 早早拦截在本地.
#
# 用法:
#   ./verify-build.sh                  # 全扫
#   ./verify-build.sh -j 8             # 并发跑
#   ./verify-build.sh -p split-payment # 只跑某个 package
#   ./verify-build.sh -v               # verbose: 失败时打印完整 stderr
#   ./verify-build.sh --tags e2e       # 加 build tag
#
# 退出码: 0 = 全部 PASS, 1 = 至少一个失败.

set -u
set -o pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
PKG_DIR="${ROOT}/packages"
JOBS=4
VERBOSE=0
ONLY=""
TAGS=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    -j)         JOBS="$2"; shift 2 ;;
    -p|--pkg)   ONLY="$2"; shift 2 ;;
    -v|--verbose) VERBOSE=1; shift ;;
    --tags)     TAGS="$2"; shift 2 ;;
    -h|--help)
      grep -E '^# ' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)          echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

if ! command -v go >/dev/null 2>&1; then
  echo "go: not found in PATH" >&2
  exit 2
fi

# 收集要扫的目录: 必须有 go.mod, 否则不是独立 module.
pkgs=()
for d in "$PKG_DIR"/*/; do
  name="$(basename "$d")"
  [[ -n "$ONLY" && "$name" != "$ONLY" ]] && continue
  [[ -f "$d/go.mod" ]] || continue
  pkgs+=("$d")
  # 嵌套 backend 子目录 (e.g. payment-admin-web/backend/) 也要扫.
  for sub in "$d"*/go.mod; do
    [[ -f "$sub" ]] || continue
    pkgs+=("$(dirname "$sub")/")
  done
done

if [[ ${#pkgs[@]} -eq 0 ]]; then
  echo "no packages found under $PKG_DIR" >&2
  exit 2
fi

echo "verify-build: scanning ${#pkgs[@]} package(s) with -j ${JOBS}${TAGS:+ -tags=$TAGS}"
echo

# 每个 pkg 单独跑 go build, log 写到 tmp file, 退出码累加.
tmpdir="$(mktemp -d -t verify-build.XXXXXX)"
trap 'rm -rf "$tmpdir"' EXIT

build_one() {
  local pkg="$1"
  local name="${pkg%/}"; name="${name##*/}"
  local out="$tmpdir/${name//\//_}.log"
  local build_args=( -gcflags=all=-trimpath="$pkg" )
  [[ -n "$TAGS" ]] && build_args+=( -tags="$TAGS" )

  if (cd "$pkg" && GOFLAGS=-mod=mod go build "${build_args[@]}" ./... ) >"$out" 2>&1; then
    echo "PASS $name"
  else
    echo "FAIL $name  (log: $out)"
    return 1
  fi
}
export -f build_one
export tmpdir TAGS

# xargs -P 并发. -L 1 保证一行一个参数, --null 不需要.
fails=0
results=$(printf '%s\n' "${pkgs[@]}" | xargs -P "$JOBS" -I{} bash -c 'build_one "$@"' _ {} 2>&1)
echo "$results"

# 统计
fail_count=$(echo "$results" | grep -c '^FAIL ' || true)
pass_count=$(echo "$results" | grep -c '^PASS ' || true)
echo
echo "──────────────────────────────"
echo "PASS: $pass_count   FAIL: $fail_count"

if [[ "$fail_count" -gt 0 ]]; then
  echo
  echo "Failure details:"
  for log in "$tmpdir"/*.log; do
    name="$(basename "$log" .log)"
    if grep -q . "$log" 2>/dev/null && grep -qE 'error|undefined|cannot find|missing|expected|imported and not used|no such' "$log"; then
      echo
      echo "── $name ─────────────────"
      if [[ "$VERBOSE" -eq 1 ]]; then
        cat "$log"
      else
        head -20 "$log"
        lines=$(wc -l <"$log")
        [[ "$lines" -gt 20 ]] && echo "  … (+$((lines-20)) more lines; rerun with -v)"
      fi
    fi
  done
  exit 1
fi

exit 0
