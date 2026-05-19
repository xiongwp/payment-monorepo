// Package errx 把"域层 sentinel error"翻译成调用方可识别的 error。
//
// 历史: 原本用 gRPC codes.Code + status.Error 包装, 给 admin web 一致的
// 404/400/409. Kitex 切换后内部不再用 grpc status, 临时降级为 opaque
// fmt.Errorf — code 字段保留但不参与序列化, 业务层仍能 errors.Is 找回 sentinel。
//
// 调用方在 init() 或构造时 Register 自己的 sentinel:
//
//	errx.Register(domain.ErrMerchantNotFound, errx.CodeNotFound)
package errx

import (
	"errors"
	"fmt"
	"sync"
)

// Code 仿 gRPC codes.Code 的精简 enum, 仅用于内部决策 / 测试断言。
// 真正传到客户端的目前只有 err.Error() 字符串。
type Code int32

const (
	CodeOK                 Code = 0
	CodeCanceled           Code = 1
	CodeUnknown            Code = 2
	CodeInvalidArgument    Code = 3
	CodeDeadlineExceeded   Code = 4
	CodeNotFound           Code = 5
	CodeAlreadyExists      Code = 6
	CodePermissionDenied   Code = 7
	CodeResourceExhausted  Code = 8
	CodeFailedPrecondition Code = 9
	CodeAborted            Code = 10
	CodeOutOfRange         Code = 11
	CodeUnimplemented      Code = 12
	CodeInternal           Code = 13
	CodeUnavailable        Code = 14
	CodeDataLoss           Code = 15
	CodeUnauthenticated    Code = 16
)

type entry struct {
	target error
	code   Code
}

var (
	mu       sync.RWMutex
	registry []entry
)

// Register 把 sentinel 注册为 code; 重复注册会追加 (第一个命中胜出), 所以
// 更具体的 sentinel 请先注册。
func Register(target error, code Code) {
	if target == nil {
		return
	}
	mu.Lock()
	registry = append(registry, entry{target: target, code: code})
	mu.Unlock()
}

// Clear 清空 (测试用)。
func Clear() {
	mu.Lock()
	registry = nil
	mu.Unlock()
}

// Map 把 err 转成 opaque error; 未注册的 err 退化为 CodeInternal。
// err == nil 返回 nil。
// 目前 code 不在 wire 上, 仅作内部追溯; 字符串形式与 caller 看到的一致。
func Map(err error) error {
	if err == nil {
		return nil
	}
	mu.RLock()
	defer mu.RUnlock()
	for _, e := range registry {
		if errors.Is(err, e.target) {
			return fmt.Errorf("[%s] %s", codeName(e.code), err.Error())
		}
	}
	return fmt.Errorf("%s", err.Error())
}

func codeName(c Code) string {
	switch c {
	case CodeOK:
		return "OK"
	case CodeCanceled:
		return "Canceled"
	case CodeUnknown:
		return "Unknown"
	case CodeInvalidArgument:
		return "InvalidArgument"
	case CodeDeadlineExceeded:
		return "DeadlineExceeded"
	case CodeNotFound:
		return "NotFound"
	case CodeAlreadyExists:
		return "AlreadyExists"
	case CodePermissionDenied:
		return "PermissionDenied"
	case CodeResourceExhausted:
		return "ResourceExhausted"
	case CodeFailedPrecondition:
		return "FailedPrecondition"
	case CodeAborted:
		return "Aborted"
	case CodeOutOfRange:
		return "OutOfRange"
	case CodeUnimplemented:
		return "Unimplemented"
	case CodeInternal:
		return "Internal"
	case CodeUnavailable:
		return "Unavailable"
	case CodeDataLoss:
		return "DataLoss"
	case CodeUnauthenticated:
		return "Unauthenticated"
	}
	return "Unknown"
}
