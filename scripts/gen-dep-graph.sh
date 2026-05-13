#!/usr/bin/env bash
# gen-dep-graph.sh — 扫 packages/*/go.mod 生成服务依赖图 (Mermaid).
#
# 输出到 docs/SERVICE_DEPS.md

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/docs/SERVICE_DEPS.md"

cat > "$OUT" <<'HEADER'
# Service Dependency Graph (auto-generated)

由 `scripts/gen-dep-graph.sh` 自动从 `packages/*/go.mod` 的 `replace` 关系生成.
CI 每次 PR 跑一遍,diff 出现 → 评审人意识到模块间耦合变化.

```mermaid
flowchart LR
HEADER

# 1. 收集每个 package -> 依赖列表
for gomod in "$ROOT"/packages/*/go.mod; do
    svc=$(basename "$(dirname "$gomod")")
    deps=$(awk '
        /^replace[[:space:]]*\(/{in=1; next}
        /^\)/{in=0}
        in && /xiongwp/{
            # github.com/xiongwp/foo => ../foo
            n=split($0, a, /[[:space:]]+/)
            for (i=1;i<=n;i++) {
                if (a[i] ~ /xiongwp/) {
                    sub(/.*xiongwp\//,"",a[i])
                    sub(/[[:space:]].*/,"",a[i])
                    print a[i]
                }
            }
        }
        /^replace github.com\/xiongwp/{
            for(i=1;i<=NF;i++){
                if($i ~ /xiongwp/){
                    s=$i; sub(/.*xiongwp\//,"",s)
                    print s
                }
            }
        }
    ' "$gomod" | sort -u)
    for d in $deps; do
        [ -z "$d" ] && continue
        echo "    $svc --> $d" >> "$OUT"
    done
done

cat >> "$OUT" <<'FOOTER'
```

## 健康度检查 (依赖原则)

正确方向:
- edge (api-gateway / payment-gateway) → core (payment-core / order-core)
- core → channel (payment-channel / card-payment)
- core → platform (kms-manage / risk-manage / oauth2-server)

警告:
- 任何 platform 服务依赖 core 服务 → 倒置依赖
- 任何 cycle (a→b, b→a) → 编译可过但语义上恶心
- payment-util 不该有 require 任何 packages/* 服务

跑 `scripts/dep-cycle-check.sh` 自动发现 cycle.
FOOTER

echo "Wrote: $OUT"
