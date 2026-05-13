#!/usr/bin/env bash
# new-rule.sh — 生成一条新的对账规则 (.star + fixture + README 卡片).
#
# 用法:
#   bash scripts/new-rule.sh <name> [--severity=critical|warning|info]
#
# 产物:
#   internal/catalog/scripts/<name>.star
#   internal/catalog/scripts/fixtures/<name>_happy.yaml
#   internal/catalog/scripts/fixtures/<name>_edge.yaml
#   internal/catalog/scripts/<name>.md  (规则卡片说明)

set -euo pipefail

NAME="${1:?usage: new-rule.sh <name> [--severity=critical|warning|info]}"
SEVERITY="warning"

for arg in "$@"; do
    case "$arg" in
        --severity=*) SEVERITY="${arg#*=}" ;;
    esac
done

# 合法性
if ! [[ "$NAME" =~ ^[a-z][a-z0-9_]*$ ]]; then
    echo "✗ rule name must be lowercase_with_underscores (got: $NAME)" >&2
    exit 1
fi
case "$SEVERITY" in
    critical|warning|info) ;;
    *) echo "✗ severity must be one of: critical/warning/info"; exit 1 ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CATALOG_DIR="$ROOT/internal/catalog/scripts"
FIXTURE_DIR="$CATALOG_DIR/fixtures"
mkdir -p "$FIXTURE_DIR"

RULE_FILE="$CATALOG_DIR/${NAME}.star"
HAPPY_FILE="$FIXTURE_DIR/${NAME}_happy.yaml"
EDGE_FILE="$FIXTURE_DIR/${NAME}_edge.yaml"
DOC_FILE="$CATALOG_DIR/${NAME}.md"

if [ -e "$RULE_FILE" ]; then
    echo "✗ already exists: $RULE_FILE" >&2
    exit 1
fi

# ───── 规则模板 ─────
cat > "$RULE_FILE" <<STARTPL
# ${NAME} — TODO: 一句话描述这条规则在防什么.
#
# Severity: ${SEVERITY}
#
# 输入:
#   ctx.scan(svc, table)         扫某 (service, table) 下所有事件
#   ctx.get_by_index(idx, val)   按业务 key 跨服务关联
#   ctx.get(svc, table, pk)      单条点查
#
# 输出 (return list of dict):
#   [{"type": "<tag>", "key": "<biz_key>",
#     "want": <expected>, "got": <actual>,
#     "detail": {<extra_info>}}, ...]
#
# 本地试跑:
#   make test-rule NAME=${NAME} FIXTURE=happy
#   make test-rule NAME=${NAME} FIXTURE=edge

def check(ctx):
    diffs = []

    # TODO: 写规则逻辑.
    # 示例: 扫某表 → 关联另一服务 → 比较 → 输出 diffs.
    #
    # rows = ctx.scan("payment-channel", "acquirer_tx")
    # for r in rows:
    #     # ...你的判断
    #     diffs.append({
    #         "type": "${NAME}",
    #         "key": r.get("id"),
    #         "detail": {"reason": "what went wrong"},
    #     })

    return diffs
STARTPL

# ───── happy fixture (规则应输出 0 diff) ─────
cat > "$HAPPY_FILE" <<HAPPYYAML
# Happy-path fixture: 规则在 healthy 数据上应输出 0 diff.

events: []

# expect_diffs: []   # 不写 = 不断言;写空列表 = 必须 0 diff
HAPPYYAML

# ───── edge fixture (规则应捕到问题) ─────
cat > "$EDGE_FILE" <<EDGEYAML
# Edge-case fixture: 规则应捕到 1+ diff.

events:
  # 示例:一笔 charge 但没对应的 PI
  - svc: payment-channel
    table: acquirer_tx
    pk: tx_test_1
    op: insert
    ts: 2026-05-13T00:00:00Z
    after:
      id: tx_test_1
      idempotency_key: idem_1
      amount: 1000
    indexes:
      idempotency_key: idem_1

# 期望规则输出 (按 type/key 顺序无关):
# expect_diffs:
#   - type: ${NAME}
#     key: idem_1
#     detail:
#       reason: TODO
EDGEYAML

# ───── 规则卡片 markdown (UI catalog 页用) ─────
cat > "$DOC_FILE" <<MDDOC
# ${NAME}

**Severity:** ${SEVERITY}

## What this rule checks

<!-- TODO: 一句话描述,会显示在 admin/catalog 页面卡片上 -->

## Why it matters

<!-- 不修这条规则会导致什么业务/合规后果?-->

## Required indexes / tables

<!-- 规则依赖的跨服务索引/表,缺了规则跑不动 -->

- \`<service>\` → \`<table>\` (index: \`<idx_name>\`)

## Outputs (diff types)

| type | meaning | suggested action |
|------|---------|-----------------|
| \`${NAME}\` | TODO | TODO |

## Test fixtures

- \`fixtures/${NAME}_happy.yaml\` — should produce 0 diffs
- \`fixtures/${NAME}_edge.yaml\` — should catch the problem

## Author / History

- Created: $(date +%Y-%m-%d)
MDDOC

echo "✓ created:"
echo "  $RULE_FILE"
echo "  $HAPPY_FILE"
echo "  $EDGE_FILE"
echo "  $DOC_FILE"
echo ""
echo "Next steps:"
echo "  1. Edit $RULE_FILE — implement def check(ctx)"
echo "  2. Edit $EDGE_FILE — add events that should trigger the rule"
echo "  3. make test-rule NAME=${NAME} FIXTURE=happy"
echo "  4. make test-rule NAME=${NAME} FIXTURE=edge"
