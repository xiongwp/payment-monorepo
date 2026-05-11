# Blue/Green + Automated Rollback

`argo-rollouts` 替代默认 k8s Deployment, 提供:
- **blueGreen** strategy: 新版本起来 + smoke test 通过才切流量
- **canary** strategy: 1% → 10% → 50% → 100% 滚动
- **AnalysisTemplate**: 自动看 Prometheus 指标 (error rate / latency) 决定推进 or 回滚

## 安装

```bash
kubectl create namespace argo-rollouts
kubectl apply -n argo-rollouts -f https://github.com/argoproj/argo-rollouts/releases/latest/download/install.yaml
# CLI plugin
brew install argoproj/tap/kubectl-argo-rollouts
```

## 用法 (oauth2-server 例)

```bash
# 把 Deployment 改成 Rollout (yaml 改 kind)
kubectl apply -f deploy/bluegreen/oauth2-server-rollout.yaml
kubectl apply -f deploy/bluegreen/analysis-template-success-rate.yaml

# 部署新版本 (改 image tag)
kubectl argo rollouts set image oauth2-server oauth2-server=ghcr.io/example/oauth2-server:v1.2.3

# 观察推进 (实时 TUI)
kubectl argo rollouts get rollout oauth2-server --watch

# 手动操作
kubectl argo rollouts promote oauth2-server     # 进入下一步
kubectl argo rollouts abort   oauth2-server     # 立即回滚
kubectl argo rollouts retry   oauth2-server     # AnalysisRun 失败后重试
```

## 自动回滚触发器 (AnalysisTemplate)

`success-rate` analysis 每 30s 跑一次, 连续 3 次失败 → 自动 abort + 回到上版本:

```promql
# 失败定义: 错误率 > 2% (相对值)
1 - (
  sum(rate(http_requests_total{service="oauth2-server", status=~"5.."}[1m]))
   /
  sum(rate(http_requests_total{service="oauth2-server"}[1m]))
) < 0.98
```

p99 latency 不超 1s:
```promql
histogram_quantile(0.99,
  sum(rate(http_request_duration_seconds_bucket{service="oauth2-server"}[1m])) by (le)
) < 1.0
```

## 回滚 SOP

| 触发 | 自动行为 | 人工 (如有需要) |
|---|---|---|
| AnalysisRun 失败 3 次 | rollout 自动 abort + scale stable=100% | 看 logs + Slack channel |
| 手动 `kubectl argo rollouts abort` | scale preview=0 + stable=100% | postmortem |
| 已经 100% promote 后才发现问题 | `kubectl argo rollouts undo <name>` | 找回上一版本 hash |

## 关键 manifest

见目录下:
- `oauth2-server-rollout.yaml` — Rollout (blueGreen strategy)
- `payment-gateway-rollout.yaml` — Rollout (canary, 5 步)
- `analysis-template-success-rate.yaml` — 成功率 + p99 latency 双指标
- `analysis-template-error-budget.yaml` — error budget burn rate (用本会话的 burn-rate.yaml)
