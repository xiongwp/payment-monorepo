# clearing-settlement

T+1 批量清算 / 结算服务。详见 [CLAUDE.md](./CLAUDE.md)。

## 端口

| 端口 | 用途 |
|---|---|
| 9890 | gRPC `SettlementService`（待实现） |
| 9891 | Admin HTTP（探针 + 触发结算 + 状态查询） |
| 9892 | Prometheus `/metrics` |

## Admin 端点

```bash
# 健康
curl http://localhost:9891/admin/health

# 触发结算
curl -X POST -H "X-Admin-Token: $TOKEN" -H "Content-Type: application/json" \
     -d '{"settle_date":"2026-04-28","currency":"PHP"}' \
     http://localhost:9891/admin/settle/trigger

# 查询状态
curl -H "X-Admin-Token: $TOKEN" \
     "http://localhost:9891/admin/settle/status?settle_date=2026-04-28&run_id=1"

# 重派卡死 PROCESSING merchant
curl -X POST -H "X-Admin-Token: $TOKEN" -H "Content-Type: application/json" \
     -d '{"settle_date":"2026-04-28","run_id":1,"threshold_seconds":300}' \
     http://localhost:9891/admin/settle/resume
```

## 当前状态

骨架：admin HTTP + skeleton service（`TriggerSettlement` 仅返回 run_id，无实际结算）。
后续 PR 接入：

- repository 层（`settlement_run` / `settlement_record` 表）
- accounting-grpc-api 客户端
- gRPC `SettlementService` 实现
- cron scheduler（等当天 day-cut 完成 → 触发）
- 商户结算规则热重载

## 构建

```bash
make build
make test                  # go test -race ./...
GITHUB_TOKEN=ghp_xxx docker build --secret id=GITHUB_TOKEN,env=GITHUB_TOKEN .
```
