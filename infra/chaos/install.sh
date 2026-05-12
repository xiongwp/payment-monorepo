#!/usr/bin/env bash
# install.sh — 部署 chaos-mesh + 注入示例实验.
set -euo pipefail

NAMESPACE="${NAMESPACE:-chaos-mesh}"

echo "Installing chaos-mesh..."
helm repo add chaos-mesh https://charts.chaos-mesh.org 2>/dev/null || true
helm upgrade --install chaos-mesh chaos-mesh/chaos-mesh \
    --namespace "$NAMESPACE" --create-namespace \
    --version 2.6.3 \
    --set dashboard.create=true \
    --set dashboard.securityMode=true

echo ""
echo "✓ chaos-mesh installed. Dashboard: kubectl port-forward -n $NAMESPACE svc/chaos-dashboard 2333:2333"
echo ""
echo "实验示例已放 experiments/. 周度自动跑 (cron):"
echo "  kubectl apply -f experiments/scheduled.yaml"
