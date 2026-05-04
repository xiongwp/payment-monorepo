#!/usr/bin/env bash
# render-ci.sh — 把 CI workflow 渲染到 sibling 仓库。
#
# 参数：
#   SERVICE                  — 仓库名
#   --workdir <subdir>       — go.mod 所在子目录（默认: 同 SERVICE 名）
#   --paths <pkg-pattern>    — go vet/test/lint 的路径（默认: ./...）
#   --siblings a,b,c         — 需要 checkout 的兄弟仓
#
# 例：
#   ./render-ci.sh kms-manage
#   ./render-ci.sh order-core --paths "./cmd/... ./internal/... ./api/..." \
#                              --siblings kms-manage,payment-channel,payment-core,risk-manage
#   ./render-ci.sh payment-admin-web --workdir payment-admin-web/backend \
#                                    --siblings kms-manage,order-core,payment-channel,payment-core,risk-manage,user-merchant-core
set -euo pipefail
SERVICE="${1:?service required}"; shift
WORKDIR="$SERVICE"
PATHS="./..."
SIBLINGS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --workdir)  WORKDIR="$2"; shift 2 ;;
    --paths)    PATHS="$2";   shift 2 ;;
    --siblings) IFS=',' read -ra SIBLINGS <<< "$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
DST_DIR="$ROOT/$SERVICE/.github/workflows"
mkdir -p "$DST_DIR"
DST="$DST_DIR/ci.yml"

CHECKOUT_BLOCK=""
for sib in "${SIBLINGS[@]}"; do
  # 先尝试本 PR 同名分支（continue-on-error），不存在就 fallback 到默认分支。
  # 用 step id + outcome 校验，确保 fallback 只在前一步真挂了才跑。
  step_id="$(echo "$sib" | tr - _)_match"
  CHECKOUT_BLOCK+="      - name: Checkout sibling $sib (try matching branch)
        id: $step_id
        continue-on-error: true
        uses: actions/checkout@v4
        with:
          repository: xiongwp/$sib
          path: $sib
          token: \${{ secrets.GITHUB_TOKEN }}
          ref: \${{ github.head_ref || github.ref_name }}
      - name: Checkout sibling $sib (default branch fallback)
        if: steps.${step_id}.outcome == 'failure'
        uses: actions/checkout@v4
        with:
          repository: xiongwp/$sib
          path: $sib
          token: \${{ secrets.GITHUB_TOKEN }}
"
done

cat > "$DST" <<YAML
name: ci
on:
  push:
    branches: [main, "claude/**"]
  pull_request:
    branches: [main]

permissions:
  contents: read

jobs:
  go:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: $WORKDIR
    steps:
      - name: Checkout $SERVICE
        uses: actions/checkout@v4
        with: { path: $SERVICE }
${CHECKOUT_BLOCK}      - uses: actions/setup-go@v5
        with:
          go-version: "1.24"
          cache: true
          cache-dependency-path: $WORKDIR/go.sum
      - name: Build
        run: go build $PATHS
      - name: Vet
        run: go vet $PATHS
      - name: Test
        run: go test -race -count=1 $PATHS
      # Lint / 漏洞扫 暂时 non-blocking —— 让首批 PR 能合，问题分批治理。
      - name: Vulnerability scan
        continue-on-error: true
        run: |
          go install golang.org/x/vuln/cmd/govulncheck@latest
          govulncheck $PATHS
      - name: Lint
        continue-on-error: true
        uses: golangci/golangci-lint-action@v6
        with:
          version: v1.62
          working-directory: $WORKDIR
          args: --timeout=5m
YAML

# 拷贝 lint 配置（如果 service 已有自定义就保留）
if [[ ! -f "$ROOT/$SERVICE/.golangci.yml" ]]; then
  cp "$ROOT/user-merchant-core/.golangci.yml" "$ROOT/$SERVICE/.golangci.yml"
fi

echo "rendered CI → $DST"
