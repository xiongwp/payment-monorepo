#!/usr/bin/env bash
# watch-rules.sh — 监 catalog/scripts/*.star,改了自动 POST 到 admin reload + 跑 happy fixture.
#
# 用法:
#   bash scripts/watch-rules.sh <catalog_dir> <admin_url>
#
# 依赖:
#   - fswatch (macOS: brew install fswatch)
#   - inotifywait (Linux: apt install inotify-tools)
#   - curl
#   - bin/recon-cli (make build-cli 一下)
#
# 行为:
#   - 改 foo.star → 跑 lint → POST /api/v1/scripts/foo/validate
#                  → 若 happy fixture 存在,自动跑一遍 → 打印结果
#   - 改 foo_happy.yaml / foo_edge.yaml → 触发对应规则重跑

set -euo pipefail

CATALOG_DIR="${1:-internal/catalog/scripts}"
ADMIN_URL="${2:-http://localhost:8080}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLI="$ROOT/bin/recon-cli"
FIXTURE_DIR="$CATALOG_DIR/fixtures"

if [ ! -x "$CLI" ]; then
    echo "✗ $CLI not found. Run 'make build-cli' first." >&2
    exit 1
fi

# 选 watcher
if command -v fswatch >/dev/null 2>&1; then
    WATCHER="fswatch -0 -r $CATALOG_DIR"
elif command -v inotifywait >/dev/null 2>&1; then
    WATCHER="inotifywait -m -r -e modify,create -q --format %w%f $CATALOG_DIR"
else
    echo "✗ install fswatch (macOS) or inotify-tools (Linux) first." >&2
    exit 1
fi

echo "watching $CATALOG_DIR ..."
echo "  admin: $ADMIN_URL"
echo "  cli:   $CLI"

# 取规则名:
#   - foo.star               → foo
#   - foo_happy.yaml         → foo
#   - foo_edge.yaml          → foo
rule_of() {
    local f="$1"
    f="$(basename "$f")"
    case "$f" in
        *.star) echo "${f%.star}" ;;
        *_happy.yaml|*_edge.yaml|*_replay_*.yaml)
            echo "$f" | sed -E 's/_(happy|edge|replay_[0-9]+)\.yaml$//' ;;
        *) echo "" ;;
    esac
}

# Debounce: 同一规则 500ms 内多次触发只处理一次
declare -A LAST_TS
handle() {
    local file="$1"
    local rule
    rule="$(rule_of "$file")"
    [ -z "$rule" ] && return
    local now_ms last
    now_ms=$(date +%s%3N 2>/dev/null || python3 -c 'import time; print(int(time.time()*1000))')
    last="${LAST_TS[$rule]:-0}"
    if [ $((now_ms - last)) -lt 500 ]; then
        return
    fi
    LAST_TS[$rule]=$now_ms

    echo ""
    echo "── changed: $file (rule=$rule) ──"

    # 1) lint 当前规则
    local star_file="$CATALOG_DIR/$rule.star"
    [ ! -f "$star_file" ] && return
    if ! "$CLI" rule lint --catalog="$CATALOG_DIR" 2>&1 | grep -E "$rule(.star)?"; then
        :
    fi

    # 2) 跑 happy fixture (若存在)
    local happy="$FIXTURE_DIR/${rule}_happy.yaml"
    if [ -f "$happy" ]; then
        "$CLI" rule test --script="$star_file" --fixture="$happy" || true
    fi

    # 3) 通知 admin reload (软失败,不阻塞 dev loop)
    if curl -fsS --max-time 3 -X POST \
        "${ADMIN_URL}/api/v1/scripts/${rule}/_reload" -d '{}' \
        -H 'Content-Type: application/json' > /dev/null 2>&1; then
        echo "  ✓ admin reloaded"
    fi
}

# 主循环
if command -v fswatch >/dev/null 2>&1; then
    fswatch -0 -r "$CATALOG_DIR" | while IFS= read -r -d '' file; do
        handle "$file"
    done
else
    inotifywait -m -r -e modify,create -q --format '%w%f' "$CATALOG_DIR" | while read -r file; do
        handle "$file"
    done
fi
