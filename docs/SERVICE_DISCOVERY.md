# 服务注册与发现 (etcd-based)

全 monorepo 的 Kitex 服务用 **etcd v3** 做服务注册和发现,跟旧 gRPC 时代
`payment-util/serviceregistry` 用同一份数据,数据格式完全兼容。

## 数据布局

etcd key 平铺,无业务前缀:

```
<service-name>/<advertise-addr>  →  {"Addr":"<host>:<port>","Metadata":null}
```

例:

```
accounting-service/accounting-service:50051  →  {"Addr":"accounting-service:50051"}
accounting-service/10.0.0.5:50051            →  {"Addr":"10.0.0.5:50051"}
user-merchant-core/user-merchant-core:9191   →  {"Addr":"user-merchant-core:9191"}
```

多副本(prod 部署 replicas=N)时同一个 `<service-name>/` 前缀下有多条 key,
client 端 Kitex 会做负载均衡。

## 关键 env 变量

| env | server | client | 含义 |
|---|---|---|---|
| `REGISTRY_ENDPOINTS` | ✓ | ✓ | etcd 地址,`etcd:2379` 或 `etcd1:2379,etcd2:2379` |
| `ADVERTISE_HOST` | ✓ | ✗ | 注册时广播的 host,prod 用 `POD_IP`,dev 留空走 container hostname |
| `SERVER_PORT` | ✓ | ✗ | 注册时广播的 port,留空走默认 |

**REGISTRY_ENDPOINTS 空时的行为:**
- server: 不注册到 etcd(但服务能起来,只是上游发现不到)
- client: fallback 静态 host:port(`kitexutil.defaultHosts` 表里的 docker DNS 名)

## Server 端接入(一行)

```go
import "github.com/xiongwp/payment-util/kitexutil"

srvOpts := []kitexserver.Option{kitexserver.WithServiceAddr(tcpAddr)}
srvOpts = append(srvOpts, kitexutil.DefaultServerOptions("accounting-service", "accounting-service:50051")...)
srv := kitexserver.NewServer(srvOpts...)
```

第 1 个参数(`svcName`)写**docker DNS 名 / k8s Service 名**,不是 Go 包名。
第 2 个参数(`advertiseAddr`)是注册广播的 host:port,必须从 caller 容器
可路由 — dev 通常等于 `<svcName>:<port>`,prod 用 `POD_IP:<port>`。

## Client 端接入(一行)

```go
cli, _ := accountingservice.NewClient("accounting-service",
    kitexutil.DefaultClientOptions("accounting-service")...,
)
```

`DefaultClientOptions` 内部按 `REGISTRY_ENDPOINTS` env 自动选路:
- 非空 → etcd discovery(`EtcdResolver`)
- 空 → 静态 host:port

注意:`svcName` 参数必须跟 server 端 `DefaultServerOptions` 第 1 个参数同名。

## docker-compose 模板

```yaml
services:
  my-service:
    environment:
      REGISTRY_ENDPOINTS: "etcd:2379"
      ADVERTISE_HOST: "my-service"     # = container_name; prod 用 ${POD_IP}
      SERVER_PORT: "9091"
    depends_on:
      etcd:
        condition: service_healthy
```

`etcd` 容器由 `packages/etcd/docker-compose.yml` 独立 stack 提供,
`deploy-stack.sh up` 会先起 etcd 再起业务服务。

## 运维 / 排错

```bash
# 看 etcd 里所有注册的实例
docker exec etcd etcdctl get --prefix '' --keys-only

# 看某个服务的所有实例
docker exec etcd etcdctl get --prefix 'accounting-service/' --keys-only

# 看实例 value(metadata 等)
docker exec etcd etcdctl get --prefix 'accounting-service/' -w json | jq

# 实时监控变化(实例上线/下线)
docker exec etcd etcdctl watch --prefix 'accounting-service/'

# 强删一个 stuck 实例(实例 OOM 后 lease 还没过期前的紧急清理)
docker exec etcd etcdctl del 'accounting-service/<bad-addr>'
```

## 故障模式

| 场景 | 现象 | 处理 |
|---|---|---|
| etcd 单节点挂了 | 所有 client 拿到的实例列表是缓存的(`EtcdResolver` 内部 `cache` map);新实例上线感知不到,但已知实例还能调 | 单节点 dev 重启 etcd,client 自动重连。prod 用 3 节点 quorum,不会单点 |
| 服务进程 OOM kill | TTL 30s 后 lease 失效,etcd 自动删 endpoint,caller 5s 内感知摘除 | 自愈,无需人工 |
| 服务 graceful stop | `Deregister` 主动 revoke lease,瞬时摘除 | 自愈 |
| 注册时 etcd 不通 | `DefaultServerOptions` 返 nil → server 不注册但仍起来 | log 报警,重启服务等 etcd 恢复 |
| caller 拿不到 endpoint | `ErrNoEndpoints` → Kitex log error,RPC 报 `no resolver available` | 检查 etcd 数据(上面 etcdctl 命令);确认 server 端注册成功 |

## 命名规约(避坑)

历史包袱:Go 包名 ≠ docker DNS 名。务必用 docker DNS 名 / k8s service 名注册和解析:

| Go 包名 | docker DNS / 注册名 |
|---|---|
| `accounting-system` | **`accounting-service`** |
| `kms-manage` | **`kms`** |
| `id-generator` | **`id-service`** |
| 其他 9 个 | 跟包名一致 |

`payment-util/kitexutil/hostports.go::defaultHosts` 表是 source-of-truth。
