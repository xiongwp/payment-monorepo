// Package trace 过渡层；真实实现在 github.com/xiongwp/payment-util/trace。
// 新代码请直接 import github.com/xiongwp/payment-util/trace，避免一次性大改
// internal/grpc 等已经用 trace.UnaryServerInterceptor 的地方；这里做薄壳转发。
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
	Logger                 = putil.Logger
	UnaryServerInterceptor = putil.UnaryServerInterceptor
	UnaryClientInterceptor = putil.UnaryClientInterceptor
	InitOTel               = putil.InitOTel
)
