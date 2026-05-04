#!/usr/bin/env bash
# render.sh — 把 templates/service.yaml 渲染成各 service 可 apply 的 manifest。
#
# 用法：
#   bash deploy/k8s/render.sh                  # 输出到 deploy/k8s/rendered/
#   bash deploy/k8s/render.sh | kubectl apply -f -
#
# IMAGE_REGISTRY 环境变量控制镜像前缀，默认 registry.example.com。

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TEMPLATE="$HERE/templates/service.yaml"
OUT="$HERE/rendered"
mkdir -p "$OUT"

REGISTRY="${IMAGE_REGISTRY:-registry.example.com}"
TAG="${IMAGE_TAG:-0.1.0}"

# 每个 service：name | grpc_port | metrics_port | replicas
SERVICES=(
  "order-core|9091|9290|2"
  "payment-core|9090|9190|2"
  "payment-channel|9092|9192|2"
  "user-merchant-core|9191|9291|2"
  "risk-manage|9490|9590|2"
  "kms-manage|9290|9390|2"
  "accounting-system|50051|9090|2"
  "accounting-batchtask|0|0|2"   # 没 gRPC，只跑 cron；模板的 probe 需要单独裁
)

for spec in "${SERVICES[@]}"; do
  IFS='|' read -r name grpc metrics replicas <<<"$spec"
  echo "→ rendering $name"
  out="$OUT/$name.yaml"
  sed -e "s/\${SERVICE}/$name/g" \
      -e "s|\${IMAGE}|$REGISTRY/$name:$TAG|g" \
      -e "s/\${GRPC_PORT}/$grpc/g" \
      -e "s/\${METRICS_PORT}/$metrics/g" \
      -e "s/\${REPLICAS}/$replicas/g" \
      "$TEMPLATE" >"$out"
done

echo
echo "渲染完成，输出在 $OUT/"
echo "用 kubectl apply -f $OUT/ 部署。"
