# payment-platform Helm Chart

完整支付平台 30+ 服务一键部署. helm install 出整栈.

## 部署

```bash
# 1) 准备 secrets (out-of-band; 真生产用 external-secrets / sealed-secrets)
kubectl create secret generic oauth2-server-secrets \
  --from-literal=OAUTH2_ADMIN_TOKEN='...' \
  -n payment

# ... 类似给每个服务

# 2) 安装
helm install pp ./chart \
  --namespace payment --create-namespace \
  --set global.image.tag=$(git rev-parse --short HEAD) \
  --values prod.yaml

# 3) 验证
kubectl get pods -n payment
kubectl logs -n payment -l app.kubernetes.io/part-of=payment-platform --tail=10
```

## 升级

```bash
helm upgrade pp ./chart -n payment \
  --set global.image.tag=$(git rev-parse --short HEAD) \
  --values prod.yaml
```

## 关闭单服务

```yaml
# overrides.yaml
services:
  tax-reporting:
    enabled: false   # 关掉它
```

```bash
helm upgrade pp ./chart -n payment -f overrides.yaml
```

## 灰度

```bash
# v2 canary, 跑 10% 流量
helm install pp-canary ./chart \
  --set global.image.tag=v2-canary \
  --set services.payment-core.replicas=1 \
  --set "services.payment-core.env.CANARY=true" \
  -n payment
# 用 service mesh / nginx ingress canary annotation 切 10%
```

## values.yaml 结构

```
global:
  env, namespace, image, securityContext, networkPolicy, ...

services:
  <name>:
    enabled, replicas, port, resources, env, hpa, ingress, cron, ...

secrets:
  oauth2AdminToken, vaultDekHex, ...
```

每个服务的 enabled / replicas / hpa 都可独立调.

## 渲染验证

```bash
helm lint ./chart
helm template ./chart > /tmp/rendered.yaml
kubectl apply --dry-run=client -f /tmp/rendered.yaml
```

CI 自动跑 (`oneshot-deploy.yml` 的 helm-lint job).

## 真生产 checklist

- [ ] image tag 别用 `latest` (用 git sha)
- [ ] Secrets 走 external-secrets / sealed-secrets (不要 plain `--set`)
- [ ] Ingress TLS cert (cert-manager letsencrypt)
- [ ] HPA 上限符合容量规划
- [ ] PDB minAvailable 跟 SLA 对齐
- [ ] ServiceMonitor 跟 Prometheus operator 安装命名空间一致
- [ ] NetworkPolicy 默认 deny + 显式 allow (生产已开)
- [ ] OTel endpoint 指向真 collector
- [ ] AuditLog endpoint 指向真 audit-log service

## 测试

```bash
# 在 minikube / kind 跑
helm install pp ./chart -n payment --create-namespace \
  --set global.image.registry=ghcr.io/example \
  --set global.image.tag=latest

# 监控启动
kubectl get pods -n payment -w
```
