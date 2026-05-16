#!/usr/bin/env bash
# dep-cycle-check.sh — 检查 packages 间是否有循环依赖.
# 用 go list / tsort 简易实现.

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

EDGES=$(mktemp)
trap 'rm -f $EDGES' EXIT

for gomod in "$ROOT"/packages/*/go.mod; do
    svc=$(basename "$(dirname "$gomod")")
    awk -v src="$svc" '
        /^replace[[:space:]]*\(/{in=1; next}
        /^\)/{in=0}
        (in || /^replace github.com\/xiongwp/) && /xiongwp/{
            for(i=1;i<=NF;i++){
                if($i ~ /xiongwp/){
                    s=$i; sub(/.*xiongwp\//,"",s); sub(/[[:space:]].*/,"",s)
                    if (s != src && s != "") print src" "s
                }
            }
        }
    ' "$gomod"
done | sort -u > "$EDGES"

# tsort: 检测 cycle
if ! tsort < "$EDGES" >/dev/null 2>&1; then
    echo "✗ Dependency cycle detected!"
    tsort < "$EDGES" 2>&1 | grep "cycle in data" -A 30 || true
    exit 1
fi
echo "✓ No dependency cycles"
