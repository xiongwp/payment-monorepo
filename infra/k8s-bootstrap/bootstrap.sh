#!/usr/bin/env bash
# bootstrap.sh — terraform apply 后跑这个把 K8s addon 装齐.
#
# 顺序很关键:
#   1. ingress-nginx (要先有, 给 ALB / CLB target)
#   2. cert-manager (要先有, 给 Ingress 自动签 cert)
#   3. prometheus-operator (要先有 ServiceMonitor CRD)
#   4. external-secrets (从 AWS Secrets Manager 拉 secrets)
#   5. argo-cd (可选 — GitOps deploy)
#   6. helm install pp ./chart (应用)

set -euo pipefail

ENV="${1:-staging}"
echo "Bootstrapping K8s addons for env=$ENV"

# ── ingress-nginx ──
echo "[1/5] ingress-nginx"
helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx 2>/dev/null || true
helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
    --namespace ingress-nginx --create-namespace \
    --set controller.service.type=LoadBalancer \
    --set controller.metrics.enabled=true \
    --set controller.podAnnotations."prometheus\.io/scrape"=true

# ── cert-manager ──
echo "[2/5] cert-manager"
helm repo add jetstack https://charts.jetstack.io 2>/dev/null || true
helm upgrade --install cert-manager jetstack/cert-manager \
    --namespace cert-manager --create-namespace \
    --set installCRDs=true \
    --version v1.13.3

echo "  waiting for cert-manager webhook..."
kubectl rollout status deployment cert-manager-webhook -n cert-manager --timeout=180s
kubectl apply -f cert-manager.yaml

# ── prometheus-operator ──
echo "[3/5] prometheus-operator"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true
helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
    --namespace monitoring --create-namespace \
    --set grafana.adminPassword="$(openssl rand -base64 32)" \
    --set alertmanager.config.global.resolve_timeout=5m

# ── external-secrets ──
echo "[4/5] external-secrets"
helm repo add external-secrets https://charts.external-secrets.io 2>/dev/null || true
helm upgrade --install external-secrets external-secrets/external-secrets \
    --namespace external-secrets --create-namespace

# ── argo-cd (可选 GitOps) ──
if [ "${INSTALL_ARGOCD:-no}" = "yes" ]; then
    echo "[5/5] argo-cd"
    helm repo add argo https://argoproj.github.io/argo-helm 2>/dev/null || true
    helm upgrade --install argocd argo/argo-cd \
        --namespace argocd --create-namespace \
        --set configs.params.server\\.insecure=true
fi

echo ""
echo "✓ Bootstrap complete. Now:"
echo "  cd ../../chart"
echo "  helm install pp . --namespace payment --create-namespace \\"
echo "    --values ../infra/k8s-bootstrap/$ENV.yaml"
