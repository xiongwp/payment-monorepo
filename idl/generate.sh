#!/usr/bin/env bash
# generate.sh — monorepo Kitex 生成脚本.
#
# 用法: 在 monorepo 根跑 `./idl/generate.sh` (或单独跑某 svc: `./idl/generate.sh order`)
#
# 依赖:
#   go install github.com/cloudwego/kitex/tool/cmd/kitex@latest
#   go install github.com/cloudwego/thriftgo@latest
#
# 行为:
#   - 对每个 idl/<svc>/v1/<svc>.proto 跑 kitex 生成 kitex_gen
#   - 输出到对应的 packages/<svc>/kitex_gen/
#   - server 端 stub + client 端 stub 一并产出

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# svc → package 目录 映射 (proto idl 名 → package 目录)
declare -A SVC_PKG=(
  [accounting]=accounting-system
  [cardcenter]=card-center
  [cardpayment]=card-payment
  [channel]=payment-channel
  [configcenter]=config-center
  [idgen]=id-generator
  [kms]=kms-manage
  [order]=order-core
  [paymentcore]=payment-core
  [risk]=risk-manage
  [splitpayment]=split-payment
  [usermerchant]=user-merchant-core
)

TARGET="${1:-all}"

generate_one() {
  local svc=$1
  local pkg=${SVC_PKG[$svc]:-}
  if [[ -z "$pkg" ]]; then
    echo "unknown svc: $svc" >&2
    return 1
  fi
  echo "─── generating kitex for $svc → packages/$pkg/kitex_gen ───"
  cd "$ROOT/packages/$pkg"
  for proto in "$ROOT/idl/$svc"/v1/*.proto; do
    if [[ ! -f "$proto" ]]; then
      continue
    fi
    echo "  kitex: $proto"
    kitex -module "reconcile-system/packages/$pkg" \
          -gen-path kitex_gen \
          -service "$svc-service" \
          "$proto"
  done
}

if [[ "$TARGET" == "all" ]]; then
  for svc in "${!SVC_PKG[@]}"; do
    generate_one "$svc"
  done
else
  generate_one "$TARGET"
fi

echo "✓ kitex generation done"
