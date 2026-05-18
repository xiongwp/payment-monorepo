// Package clients — accounting.go 已删除.
//
// 旧 AccountingClient stub (PostMovements / PostSplitAtomic / ReverseSplit /
// GetBalance) 在 SP-AC-7 之后已经无 caller (老路径 execute.go 同步删除),
// engine.go / saga_step.go 的 Accounting 字段一并移除.
//
// 当前生产路径走 AccountingGRPCClient (accounting_grpc.go) → 新 gRPC
// TransactionService.CreateTransaction.
package clients
