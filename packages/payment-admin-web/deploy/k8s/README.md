# K8s 多 pod 部署模板

每个微服务复制一份 `templates/service.yaml`，把 `${SERVICE}` / `${IMAGE}` /
`${GRPC_PORT}` 等占位符替换。

## 前置依赖

集群里已经跑：
- etcd（StatefulSet 3 节点 — 服务发现 + leader election）
- shared-meta + shared-shard-0..9 MySQL（StatefulSet）
- redis（master + 2 replica + 3 sentinel，accounting-system 用 sentinel 模式）
- 应用要的中间件（kafka / clickhouse / nebulagraph 视服务而定）

K8s 集群最低 1.27（用到 `coordination.k8s.io/Lease` API；我们这里其实走 etcd
不依赖 K8s lease，但 HPA v2 / topologySpreadConstraints 都要这个版本以上）。

## 模板覆盖范围

`templates/service.yaml` 一份 manifest 包含：
- `Deployment`：默认 `replicas: 2`，HPA 接管后由它决定
- `Service`：ClusterIP，gRPC 端口 + metrics 端口，给集群内其它服务调用
- `ConfigMap`：service 的 yaml config（registry.endpoints 指向 etcd-svc:2379）
- `HorizontalPodAutoscaler`：CPU 70% / mem 80% 触发扩缩，min=2 max=10
- `PodDisruptionBudget`：滚动 / 节点维护时至少保留 1 个 pod 可用
- `topologySpreadConstraints`：副本散布到不同 node / zone

## 用法

```bash
# 替换占位符并 apply
sed -e 's/${SERVICE}/order-core/g' \
    -e 's|${IMAGE}|registry.example.com/order-core:0.1.0|g' \
    -e 's/${GRPC_PORT}/9091/g' \
    -e 's/${METRICS_PORT}/9290/g' \
    deploy/k8s/templates/service.yaml | kubectl apply -f -
```

或者 helm-ify。每个仓自己再加 `deploy/k8s/<service>/` 把 ConfigMap / Secret
内容塞进去。

## 多副本 + leader election 工作机制

1. Deployment 起 N 个 pod
2. 每个 pod 启动后 `serviceregistry.RegisterSelf` 把 `(service, pod_ip:port)`
   写到 etcd `/services/<service>/`，TTL 10s + 心跳
3. 调用方走 `etcd:///<service>` resolver 拿到 N 个 IP，round_robin 均摊
4. cron worker 用 `serviceregistry.RunLeaderLoop` 抢 etcd 的 lease key，只有
   leader pod 真跑 cron；leader 挂了下一副本秒级接管
5. K8s readiness probe 让滚动升级期间下线的 pod 立刻摘流量

## 故障注入演练

```bash
# 删一个 pod，验证另一个秒级接管 leader（看 etcd 里 /leader/order-core/* 的 value 变化）
kubectl delete pod order-core-xxxx
docker exec etcd etcdctl get --prefix /leader/order-core/

# 滚动升级，确认请求 0 失败
kubectl rollout restart deployment/order-core
# 同时跑负载脚本观察成功率（API gateway 上 grpc_request_total{code="OK"}）
```
