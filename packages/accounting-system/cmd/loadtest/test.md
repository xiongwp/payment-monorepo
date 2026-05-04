# 完整压测（建账 + 充值 + 100万笔）
go run -mod=vendor ./cmd/loadtest

docker ps | grep accounting-system

# 仅建账（保存账户到文件）
go run -mod=vendor ./cmd/loadtest -setup-only

# 复用已有账户跑压测（不重新建账）
go run -mod=vendor ./cmd/loadtest -txn-only

# 自定义参数
go run -mod=vendor ./cmd/loadtest \
  -addr localhost:50051 \
  -users 90000 -merchants 10000 \
  -txns 1000000 -workers 300 \
  -fee1 0.01 -fee2 0.02 \
  -min-amt 10 -max-amt 1000



curl http://localhost:8888/admin/tcc/retry-confirm -d '{"threshold_seconds":0}'