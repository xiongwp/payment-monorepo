# split-payment CI/CD (SP-AC-7 PH3-4)

## Architecture

```
git push tag split-payment/v1.0.3
        │
        ▼
GitHub Actions (release.yml)
  ├─ go test
  ├─ docker buildx → ghcr.io/<owner>/split-payment:1.0.3
  ├─ cosign sign (keyless via OIDC)
  └─ PR to payment-gitops repo (yq bump kustomization image tag)
        │
        ▼
ArgoCD watches payment-gitops main
  ├─ dev/staging  → auto-sync (selfHeal + prune)
  └─ prod         → manual sync after oncall merges PR
        │
        ▼
K8s Deployment rolls (maxSurge=1, maxUnavailable=0)
  ├─ ExternalSecret pulls Vault secrets → split-payment-secrets
  ├─ cert-manager rotates mTLS cert → split-payment-mtls
  └─ Reloader monitors secret/configmap changes → auto rollout
```

## Repo Layout

This repo (monorepo):
```
packages/split-payment/
  .github/workflows/
    ci.yml         # PR / push → test + lint + build PR image
    release.yml    # tag split-payment/v* → push + cosign + GitOps PR
  deploy/argocd/
    application.yaml   # AppProject + 3 environments
  deploy/k8s/
    deployment.yaml          # base manifests
    cert-manager-cert.yaml   # PH3-2 mTLS
    external-secrets.yaml    # PH3-3 Vault
```

GitOps repo (separate, e.g. `xiongwp/payment-gitops`):
```
envs/
  dev/split-payment/
    kustomization.yaml   # images[0].newTag, replicas, env-specific patches
  staging/split-payment/
    kustomization.yaml
  prod/split-payment/
    kustomization.yaml
base/
  split-payment/        # references this repo's deploy/k8s/ via remote URL
```

Example `envs/prod/split-payment/kustomization.yaml`:
```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: payment
resources:
  - https://github.com/xiongwp/payment-monorepo//packages/split-payment/deploy/k8s?ref=main
  - https://github.com/xiongwp/payment-monorepo//packages/split-payment/deploy/k8s/cert-manager-cert.yaml?ref=main
  - https://github.com/xiongwp/payment-monorepo//packages/split-payment/deploy/k8s/external-secrets.yaml?ref=main
images:
  - name: split-payment
    newName: ghcr.io/xiongwp/split-payment
    newTag: "1.0.3"        # <-- release.yml 通过 yq 修改这一行
replicas:
  - name: split-payment
    count: 5
patches:
  - target: { kind: Deployment, name: split-payment }
    patch: |
      - op: replace
        path: /spec/template/spec/containers/0/resources/requests/cpu
        value: 500m
      - op: replace
        path: /spec/template/spec/containers/0/resources/requests/memory
        value: 512Mi
```

## Secrets needed in GitHub repo

| Secret | 用途 |
|---|---|
| `GITHUB_TOKEN` | 自动 — push GHCR + 创建 GH Release |
| `GITOPS_PUSH_TOKEN` | PAT 推 payment-gitops repo (含 contents:write 范围) |
| `CROSS_REPO_TOKEN` | (可选) 跨私仓 checkout, 单仓不需要 |

cosign 用 GitHub OIDC, 无需 long-lived key.

## Promotion Flow

1. **Feature branch → main (PR)**
   - CI 跑 lint + test + 临时 image `ghcr.io/.../split-payment:pr-123`.
   - Reviewer 看 trivy 扫描 + coverage 报告.
   - merge → main.

2. **Release dev**
   - 不打 tag, 直接靠 ArgoCD auto-sync; main 分支 image (`sha-xxx` 或 branch tag) 自动滚到 dev.

3. **Release staging**
   - Owner 打 tag `split-payment/v1.0.3` (即使是 RC).
   - release.yml: 推 image + cosign sign + 写 PR 到 GitOps `envs/staging/split-payment`.
   - GitOps PR 自动 merge (staging 走 auto).
   - ArgoCD sync staging.

4. **Promote staging → prod**
   - Oncall review staging metrics 24h.
   - 改 `envs/prod/split-payment/kustomization.yaml` 的 newTag = 1.0.3 (手工 PR 或 cherry-pick staging PR).
   - 2-eyes approve merge.
   - ArgoCD UI 点 Sync (prod 不自动 sync), 监控滚动进度.

## Rollback

直接在 GitOps repo:
```bash
git revert <commit-that-bumped-prod>
git push
# ArgoCD UI sync → 旧 image tag 回滚.
```

或者 ArgoCD History → 选上一个 Revision → Rollback.

## 监控 CI/CD 健康

- ArgoCD UI: 监 sync_status / health_status.
- Prometheus + Grafana: ArgoCD 暴露 `argocd_app_info` / `argocd_app_sync_total`.
- 告警: `argocd_app_info{sync_status!="Synced"}` 持续 15min → page on-call.

## 应急: 绕过 ArgoCD 直接 kubectl

```bash
# 仅紧急, 之后需 git pull 把 manual change 写回 GitOps 防漂移自动回滚.
kubectl set image deployment/split-payment -n payment \
  split-payment=ghcr.io/xiongwp/split-payment:HOTFIX_TAG
```

⚠ ArgoCD selfHeal 在 staging/dev 会立即回滚 manual change. prod 不会.
