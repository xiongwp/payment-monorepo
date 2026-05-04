// Package trace 过渡层；真实实现在 github.com/xiongwp/payment-util/trace。
// 保留此薄壳让残留的 github.com/xiongwp/payment-channel/internal/trace
// import 保持能编译；新代码请直接 import github.com/xiongwp/payment-util/trace。
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
