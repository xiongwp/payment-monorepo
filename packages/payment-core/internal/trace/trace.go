// Package trace 过渡层；真实实现在 github.com/xiongwp/payment-util/trace。
// 本仓 channelclient / riskclient / kmsclient 等仍然 import
// github.com/xiongwp/payment-core/internal/trace；保留此薄壳把调用转发到
// payment-util，避免一次性大改多处 call site。新代码请直接
// import github.com/xiongwp/payment-util/trace。
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
