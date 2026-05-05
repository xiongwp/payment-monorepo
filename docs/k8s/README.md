# K8s 部署清单

`service-deployment-template.yaml` —— 通用 Deployment + Headless Service + PodDisruptionBudget 模板。

每个服务复制一份，替换 `<SVC>` / `<SVC_UPPER>` / `<PORT>` 等占位符。

## 关键约定

- **`REGISTRY_ADVERTISE_ADDR=$(status.podIP)`** 通过 downward API 注入。
  本仓 `payment-util/serviceregistry/advertise.go` 三层 fallback 第一条就读这个 env，
  K8s 部署下永远走这条 → Pod IP 写进 etcd → 集群 CNI 保证可达。
  本地 docker compose 没有 K8s，靠第二条 UDP-dial 探主网卡 IP 兜底。

- **`<SVC>_REGISTRY_ENDPOINTS=etcd-cluster.svc:2379`** 让 client 端走
  `etcd:///<svc>` 服务发现，绕开 K8s DNS 的 stale alias 问题
  （多 region / 跨集群部署时尤其关键）。

- **`terminationGracePeriodSeconds: 30`** 给 fx 的 OnStop 钩子时间：
  - serviceregistry.Registrar.Close() → revoke etcd lease（端点立即从 etcd 摘掉）
  - grpc.Server.GracefulStop() → 等 in-flight RPC 完成

- **Headless Service（`clusterIP: None`）** 不是 etcd 服务发现的替代品，
  是冗余：让 K8s DNS 也能 resolve（NetworkPolicy / 调试 / fallback 用）。
  实际生产流量走 `etcd:///<svc>` resolver。

## 已知占位符对应

| <SVC> | <SVC_UPPER> | <PORT> | <METRICS_PORT> |
|---|---|---|---|
| kms-manage | KMS | 9290 | 9390 |
| card-center | CARDCENTER | 9443 | 9543 |
| card-payment | CARDPAYMENT | 9444 | 9544 |
| user-merchant-core | USERMERCHANTCORE | 9191 | 9291 |
| order-core | ORDERCORE | 9091 | 9191（与 user-merchant 重叠，按 cluster 调） |
| payment-core | PAYCORE | 9090 | 9190 |
| payment-channel | PAYCHAN | 9092 | 9093 |
| risk-manage | RISK | 9490 | 9590 |
| accounting-system | ACCOUNTING | 50051 | 50052 |
