package server

import (
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/pkg/errx"
)

// init 在包加载时把本服务的 sentinel 注册一次; 之后 handler 里任意位置
// errx.Map(err) 会拿到一致映射, 不再需要逐处写 switch。
//
// 顺序按"更具体的先": errors.Is 能链式解包, 但 registry 命中第一个就返回,
// 所以 Validation / InvalidTransition 要排在 NotFound 前面。
func init() {
	errx.Register(domain.ErrValidation, errx.CodeInvalidArgument)
	errx.Register(domain.ErrMerchantKYCInvalidTransition, errx.CodeFailedPrecondition)
	errx.Register(domain.ErrMerchantNotFound, errx.CodeNotFound)
	errx.Register(domain.ErrMerchantSecretNotFound, errx.CodeNotFound)
}

// grpcErr 过渡名: 内部 handler 仍可调 grpcErr(err), 实质委托给 errx.Map。
func grpcErr(err error) error { return errx.Map(err) }
