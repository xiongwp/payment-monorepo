#!/usr/bin/env bash
# render.sh — 把 user-merchant-core 的 K8s 模板渲染到指定 sibling 仓库。
#
# 用法：
#   ./render.sh <service-name> <grpc-port> <metrics-port> [extra-port]
#   ./render.sh kms-manage 9290 9390
#   ./render.sh payment-channel 9092 9093 9192
#
# 输出落到 ../<service-name>/deploy/k8s/。每次运行覆盖（请 git diff 检查）。
set -euo pipefail
SERVICE="${1:?service name required}"
GRPC="${2:?grpc port required}"
METRICS="${3:?metrics port required}"
EXTRA="${4:-}"

ROOT="$(cd "$(dirname "$0")/../../../.." && pwd)"
SRC="$ROOT/user-merchant-core/deploy/k8s"
DST="$ROOT/$SERVICE/deploy/k8s"
mkdir -p "$DST"

# 把名字 / 端口换成目标值；保留所有结构（probe、HPA、PDB、NetworkPolicy）。
for f in deployment.yaml service.yaml hpa.yaml pdb.yaml servicemonitor.yaml networkpolicy.yaml configmap.yaml secret.example.yaml; do
  in="$SRC/$f"; out="$DST/$f"
  sed \
    -e "s/user-merchant-core/$SERVICE/g" \
    -e "s/9191/$GRPC/g" \
    -e "s/9291/$METRICS/g" \
    "$in" > "$out"
done

# 额外端口（webhook / etc）追加到 deployment ports + service
if [[ -n "$EXTRA" ]]; then
  echo ""                                 >> "$DST/deployment.yaml"
  echo "# rendered: extra port $EXTRA"    >> "$DST/deployment.yaml"
  echo "# (manual: add { name: extra, containerPort: $EXTRA } to container ports)" >> "$DST/deployment.yaml"
fi

echo "rendered $SERVICE → $DST"
