package server

import (
	"google.golang.org/grpc"

	"github.com/xiongwp/user-merchant-core/pkg/tracex"
)

// tracexOTelInterceptor 薄壳，把 pkg/tracex.OTelServerInterceptor 抬进 internal
// 避免 grpc.go 直接 import pkg/tracex 的 OTel 部分（控反向依赖）。
func tracexOTelInterceptor() grpc.UnaryServerInterceptor {
	return tracex.OTelServerInterceptor()
}
