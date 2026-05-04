// Package trace 过渡层；真实实现在 github.com/xiongwp/payment-util/trace。
// 保留此薄壳以便 internal/channel、internal/service 等仍 import
// github.com/xiongwp/order-core/internal/trace 的 call site 不受影响。
// 新代码请直接 import github.com/xiongwp/payment-util/trace。
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
