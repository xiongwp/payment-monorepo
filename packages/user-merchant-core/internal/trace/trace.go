// Package trace 过渡层；真实实现在 github.com/xiongwp/payment-util/trace。
// 保留本包只是避免 internal 调用方（internal/server/grpc.go 等）一次性大改；
// 新代码请直接 import github.com/xiongwp/payment-util/trace。
//
// 原层指向 pkg/tracex（同仓内），现一直接指向 payment-util/trace 跨仓共享
// 包。两者 API 签名一致；导入路径不同而已。
package trace

import (
	putil "github.com/xiongwp/payment-util/trace"
)

const (
	HeaderKey   = putil.HeaderKey
	MetadataKey = putil.MetadataKey
)

var (
	FromContext            = putil.FromContext
	WithTraceID            = putil.WithTraceID
	Inject                 = putil.Inject
	Generate               = putil.Generate
	UnaryServerInterceptor = putil.UnaryServerInterceptor
	UnaryClientInterceptor = putil.UnaryClientInterceptor
	Logger                 = putil.Logger
)
