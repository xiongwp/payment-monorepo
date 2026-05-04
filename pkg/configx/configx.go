// Package configx — 启动期配置校验。
//
// viper 的哲学是宽松读取；生产里希望"必填项缺了"是硬失败，而不是跑到 RPC
// 才 panic。configx 提供若干常用校验器 + 一个 Validator 链。
//
// 典型用法：
//
//   v := viper.New(); v.ReadInConfig()
//   if err := configx.Run(v,
//       configx.Required("database.meta.dsn"),
//       configx.PositiveDuration("timeouts.default"),
//       configx.NonNegativeInt("cache.merchant.size"),
//       configx.OneOf("auth.mode", "bearer", "mtls", "none"),
//   ); err != nil { log.Fatal(err) }
package configx

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Check 在 Run 里被逐个调用；返回 nil 即通过。
type Check func(v *viper.Viper) error

// Run 按顺序执行 checks，把失败累积成一个 joined error 返回。全部通过返回 nil。
func Run(v *viper.Viper, checks ...Check) error {
	var errs []error
	for _, c := range checks {
		if err := c(v); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return fmt.Errorf("config invalid:\n  - %s", strings.Join(msgs, "\n  - "))
}

// Required key 必须存在且非空字符串。
func Required(key string) Check {
	return func(v *viper.Viper) error {
		if !v.IsSet(key) || strings.TrimSpace(v.GetString(key)) == "" {
			return fmt.Errorf("required %s is missing", key)
		}
		return nil
	}
}

// PositiveDuration time.Duration 必须 > 0。
func PositiveDuration(key string) Check {
	return func(v *viper.Viper) error {
		d := v.GetDuration(key)
		if d <= 0 {
			return fmt.Errorf("%s must be > 0 (got %s)", key, d)
		}
		return nil
	}
}

// NonNegativeInt int >= 0（0 允许，用作"禁用"信号）。
func NonNegativeInt(key string) Check {
	return func(v *viper.Viper) error {
		n := v.GetInt(key)
		if n < 0 {
			return fmt.Errorf("%s must be >= 0 (got %d)", key, n)
		}
		return nil
	}
}

// PositiveInt int > 0。
func PositiveInt(key string) Check {
	return func(v *viper.Viper) error {
		n := v.GetInt(key)
		if n <= 0 {
			return fmt.Errorf("%s must be > 0 (got %d)", key, n)
		}
		return nil
	}
}

// OneOf 字符串值必须在白名单里。
func OneOf(key string, allowed ...string) Check {
	return func(v *viper.Viper) error {
		got := v.GetString(key)
		for _, a := range allowed {
			if got == a {
				return nil
			}
		}
		return fmt.Errorf("%s = %q not one of %v", key, got, allowed)
	}
}

// Joined 把多个 Check 合成一个（便于条件组合）。
func Joined(checks ...Check) Check {
	return func(v *viper.Viper) error {
		var errs []error
		for _, c := range checks {
			if err := c(v); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// When(pred) 仅在 pred 返回 true 时执行 c；否则 skip。
// 例：仅当 kms.endpoint 非空时校验 kms.rpc_timeout。
func When(pred func(v *viper.Viper) bool, c Check) Check {
	return func(v *viper.Viper) error {
		if !pred(v) {
			return nil
		}
		return c(v)
	}
}

// KeySet 谓词：key 已设置并且字符串非空。
func KeySet(key string) func(v *viper.Viper) bool {
	return func(v *viper.Viper) bool {
		return v.IsSet(key) && strings.TrimSpace(v.GetString(key)) != ""
	}
}

// DurationAtLeast 校验 Duration >= min。
func DurationAtLeast(key string, min time.Duration) Check {
	return func(v *viper.Viper) error {
		d := v.GetDuration(key)
		if d < min {
			return fmt.Errorf("%s must be >= %s (got %s)", key, min, d)
		}
		return nil
	}
}
