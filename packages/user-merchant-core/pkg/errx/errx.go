// Package errx 把"域层 sentinel error"翻译成 gRPC status code。
//
// 原来分散在 server/merchant.go / server/merchant_secret.go 两处的 grpcErr
// 各有一份 switch；每加一个 service 就要重写一遍。errx 提供：
//
//   1. 全局注册表 Register(err, code)，按 errors.Is 匹配命中第一个即返回
//   2. 默认集成 codes.Internal fallback
//   3. 调用方用 Map(err) 替代 switch
//
// 调用方在 init() 或构造时 Register 自己的 sentinel：
//
//   errx.Register(domain.ErrMerchantNotFound, codes.NotFound)
package errx

import (
	"errors"
	"sync"
)

type entry struct {
	target error
	code   codes.Code
}

var (
	mu       sync.RWMutex
	registry []entry
)

// Register 把 sentinel 注册为 code；重复注册会追加（第一个命中胜出），所以
// 更具体的 sentinel 请先注册。
func Register(target error, code codes.Code) {
	if target == nil {
		return
	}
	mu.Lock()
	registry = append(registry, entry{target: target, code: code})
	mu.Unlock()
}

// Clear 清空（测试用）。
func Clear() {
	mu.Lock()
	registry = nil
	mu.Unlock()
}

// Map 把 err 转成 gRPC status.Error；未注册的 err 退化为 codes.Internal。
// err == nil 返回 nil。
func Map(err error) error {
	if err == nil {
		return nil
	}
	mu.RLock()
	defer mu.RUnlock()
	for _, e := range registry {
		if errors.Is(err, e.target) {
			return status.Error(e.code, err.Error())
		}
	}
	return fmt.Errorf("%s", err.Error())
}
