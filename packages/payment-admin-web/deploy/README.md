# 全栈联合部署

把 `kms-manage / payment-channel / order-core / payment-core / payment-admin-web` 五个仓里的 docker-compose **不复制地拼起来**跑，只靠：

- 一个共享的 Docker 网络 `payment-stack`（所有容器都加进去，靠容器名互访）
- 每个服务一份 override（本目录 `overrides/`），只叠加 network / env / host 端口重绑
- 顶层脚本 `../deploy.sh` 把启动顺序和 kms 引导串起来

## 目录要求

五个仓必须同级 clone（跟 go.work / 各 repo 的 replace 约定一致）：

```
<root>/
  ├── kms-manage/
  ├── order-core/
  ├── payment-channel/
  ├── payment-core/
  └── payment-admin-web/   ← 脚本在这里
      ├── deploy.sh
      └── deploy/
          ├── README.md         （本文件）
          ├── kms-keys/         （init-kms 后生成，只读挂进 kms 容器）
          └── overrides/
              ├── kms-manage.yml
              ├── payment-channel.yml
              ├── order-core.yml
              ├── payment-core.yml
              └── payment-admin-web.yml
```

## 端口布局（host 侧，避让冲突后的最终结果）

| 服务 | 端口 | 用途 |
|---|---|---|
| payment-admin-web (nginx) | **8080** | 管理 UI |
| payment-admin-backend (BFF) | **19190** | BFF HTTP /api |
| payment-core | **9090** | gRPC |
| payment-core | **9190** | /metrics + /healthz |
| payment-channel | **9092** | gRPC AcquirerService |
| payment-channel | **9192** | HTTP /wh/{adapter}（上游 webhook 入口）|
| payment-channel | **9093** | /metrics + /healthz |
| order-core | **9091** | gRPC |
| order-core | **19090** | /metrics + /healthz（base 是 9090，override 改 19090 避让 payment-core）|
| kms-manage | **9290** | gRPC |
| kms-manage | **9390** | /metrics + /healthz |
| payment-channel MySQL | 3406–3416 | meta + 10 shard |
| order-core MySQL | 3306–3316 | meta + 10 shard |

容器互访仍用内部端口（`payment-core:9090` 等），不受 host 端口重绑影响。

## 启动顺序

`deploy.sh up` 按以下顺序拉起（每个服务依赖前一个）：

1. **payment-stack** docker network（如果还没就建）
2. **kms-manage** —— 需要先 `deploy.sh init-kms` 产 master key
3. **payment-channel** —— 11 个 MySQL + 业务容器，等 MySQL healthy
4. **order-core** —— 同样 11 个 MySQL + 业务容器
5. **payment-core** —— 无状态，连 payment-channel 和 kms-manage
6. **payment-admin-web** —— BFF + nginx 前端

## 常用命令

```bash
# 首次部署（产 kms master key 到 deploy/kms-keys/main.key）
./deploy.sh init-kms

# 拉全栈
./deploy.sh up

# 只起子集
./deploy.sh up kms-manage payment-channel

# 健康检查（TCP 探活 6 个关键端口）
./deploy.sh check

# 看某服务日志
./deploy.sh logs payment-core

# 停全栈（保留 MySQL 数据卷）
./deploy.sh down

# 停并清数据卷（测试前清零）
./deploy.sh down --volumes

# 看各服务 ps
./deploy.sh status
```

## e2e 验证清单

起完后按这个顺序手工 / 自动验证：

```bash
# 1. kms-manage 在线
curl -s http://127.0.0.1:9390/healthz     # → ok
docker exec kms-manage /usr/local/bin/kmsctl list /var/lib/kms-manage/keys

# 2. payment-channel 在线
curl -s http://127.0.0.1:9093/healthz     # → ok

# 3. payment-core 在线 + 能连到 channel + kms
curl -s http://127.0.0.1:9190/healthz     # → ok

# 4. order-core 在线
curl -s http://127.0.0.1:19090/healthz    # → ok

# 5. 管理后台
open http://localhost:8080
#    → 工作台能看到订单数（需要先有数据；没有就是 0）
#    → KMS 页面能看到一把 master key
#    → 路由探测：country=PH, method=GCASH, amount=10000 → 看到路由结果
```

## override 采用的 compose 语法

- `networks: - payment-stack` + `networks: payment-stack: external: true`
  —— 把服务加到预先建好的外部网络
- `ports: !override` —— 覆盖父 compose 的端口列表（Compose v2.24+ 支持；本项目要求）
- `extra_hosts: !reset []` —— 清掉 `host.docker.internal` 的特殊路由

如果你的 docker compose 版本不支持 `!override` / `!reset`，请升级到 >= 2.24。

## 常见问题

**Q: up 的时候 MySQL 一直 starting？**  
A: 首次 up 要导 init.sql + 建表，慢是正常。`deploy.sh logs payment-channel` 或 `deploy.sh logs order-core` 看后端是不是已经开始连 DB。

**Q: payment-core 启动报 `kms decrypt failed`？**  
A: 说明 config 里写了 `kms:v1:...` 密文但 kms-manage 的 ACTIVE key 变了。解决：要么 `init-kms` 重来 + 重新加密；要么把 yaml 改回明文。

**Q: 如何只跑某两个服务联调？**  
A: `deploy.sh up kms-manage payment-channel` 就够了，剩下的不需要。每个 override 都是独立 merge 的。

**Q: 想改 admin-web 的鉴权 token？**  
A: 改 `deploy/overrides/payment-admin-web.yml` 里的 `ADMIN_BEARER_TOKEN`，重启。
