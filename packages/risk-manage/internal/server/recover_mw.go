// recover_mw.go — Kitex 入站 panic recover middleware.
//
// 业务 handler panic → 转 error 返上游, 不挂进程; 同时 zap.Error + 栈写日志.
// payment-util/kitexutil 已有 RecoverMW(log) — 我们这里直接复用, 不再重写.
// 留这个文件只是为了把"server MW 三件套 (recover + log + tracing)" 集中在一处,
// 后续 audit / 调试时容易找到入口.
//
// MW 装载顺序 (在 grpc.go 里):
//   1. RecoverMW   — 最外层, panic 都被它吃掉
//   2. TracingMW   — 在 panic 内部, 这样 panic 时还能在 span 上打 RecordError
//   3. LogMW       — 最内层, 拿到完整 ctx (含 trace_id) 才能打日志
package server

import (
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/xiongwp/payment-util/kitexutil"
	"go.uber.org/zap"
)

// recoverMW 包一层 payment-util/kitexutil.RecoverMW; 接口签名跟 TracingServerMW
// 对齐, 方便 grpc.go 里串成统一形态.
func recoverMW(log *zap.Logger) endpoint.Middleware {
	if log == nil {
		log = zap.NewNop()
	}
	return kitexutil.RecoverMW(log)
}
