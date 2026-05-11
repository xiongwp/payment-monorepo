// redis_adapter.go — 生产用 Redis-backed 实现 (基于 asynq)。
//
// 不在本包直接 import asynq, 避免拉重依赖到所有 service。
// 业务方在 cmd/server/main.go 用:
//
//   import "github.com/hibiken/asynq"
//   import "reconcile-system/packages/payment-util/jobqueue"
//
//   client := asynq.NewClient(asynq.RedisClientOpt{Addr: "redis:6379"})
//   srv := asynq.NewServer(asynq.RedisClientOpt{...}, asynq.Config{
//       Concurrency: 50,
//       Queues: map[string]int{"critical": 6, "default": 3, "low": 1},
//       RetryDelayFunc: asynq.DefaultRetryDelayFunc,
//   })
//
//   c := jobqueue.WrapAsynqClient(client)
//   s := jobqueue.WrapAsynqServer(srv)
//
//   // 之后业务代码用 jobqueue.Client/Server 接口 (跟 MemoryClient 一致),
//   // 测试用 MemoryClient 不需要 Redis.

package jobqueue

// 此文件留作 wrapper 占位; 真正接 asynq 的代码在业务侧 cmd/server/main.go
// 用 WrapAsynqClient/Server 把 asynq 类型转成本包 interface。
//
// 这样:
//   - 测试: 用 MemoryClient (本包内, 无依赖)
//   - 生产: 业务侧 import asynq + 调 WrapXxx 转 jobqueue.Client/Server
//
// payment-util 包不强依赖 asynq, 不强依赖 redis。
