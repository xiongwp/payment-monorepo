# Argo CD GitOps — payment-platform

Declarative deploy via Argo CD App-of-Apps pattern.

## 拓扑

```
root-app  →  applications/
              ├─ core-services.yaml       (payment-core, order-core, payment-channel, accounting-system, …)
              ├─ edge-services.yaml       (api-gateway, biz-admin-web, payment-admin-web)
              ├─ recon-and-risk.yaml      (reconplatform, risk-manage, aml-screening)
              ├─ data-plane.yaml          (ha-data: redis-sentinel, mysql, kafka, clickhouse)
              ├─ observability.yaml       (prometheus, alertmanager, tempo, loki, grafana)
              └─ argocd-self.yaml         (Argo CD 自管,防止漂移)
```

每个 child Application 指向 chart/ 下的 Helm chart 或 child-chart, values 通过 `helm.parameters` / `helm.valuesFiles` 注入。

## 部署

```bash
# 集群已有 argocd namespace 后:
kubectl apply -f deploy/argocd/root-app.yaml

# 验证
argocd app list
argocd app sync payment-root
```

## 滚动 / 回滚

- Sync via git commit (推荐, 自动 sync on commit if enabled)
- Manual: `argocd app sync <name>`
- Rollback: `argocd app rollback payment-root <revision>` (revision 从 `argocd app history` 拿)

## RBAC

- `dev`: read-only across all apps
- `oncall`: sync + rollback core-services / edge-services
- `platform-admin`: full control

详见 `deploy/argocd/rbac.yaml`.
