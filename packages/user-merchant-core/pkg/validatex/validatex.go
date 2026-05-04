// Package validatex — 轻量入参校验。定位：
//
//   * 按字段校验，不做复杂组合 DSL（像 protovalidate 那样）
//   * 每个 rule 返回 (ok, reason)，失败时 reason 进到 gRPC InvalidArgument
//     的 description，方便 admin UI 直接展示
//   * 零依赖；不反射、不生成代码，手写校验也值当
//
// 典型 handler 用法：
//
//   if err := validatex.All(
//       validatex.Required("name", req.GetName()),
//       validatex.MaxLen("name", req.GetName(), 128),
//       validatex.Email("contact_email", req.GetContactEmail()),
//       validatex.CountryISO2("country", req.GetCountry()),
//   ); err != nil { return nil, err }
package validatex

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Rule 返回 nil = 通过；否则是 fmt 友好的描述。
type Rule func() error

// All 累积所有失败；若全部通过返回 nil。
// 返回 errors.Is(err, ErrValidation)==true 的 joined error。
func All(rules ...Rule) error {
	var errs []error
	for _, r := range rules {
		if err := r(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	// 把所有失败合并成一个 Validation error —— 调用方用 errors.Is 能命中，同时
	// Error() 里包含所有具体失败，方便前端展示。
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return fmt.Errorf("%w: %s", ErrValidation, strings.Join(msgs, "; "))
}

// ErrValidation 根错误；handler 里 errors.Is(err, ErrValidation) 判别。
// 调用方一般用自己的 domain.ErrValidation 作为包装，见 mapping 例子。
var ErrValidation = errors.New("validation failed")

// ─── 字段规则 ────────────────────────────────────────────────────────────────

// Required 非空字符串。
func Required(field, v string) Rule {
	return func() error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
}

// MaxLen 最长字节数（对非 ASCII 用户友好但不严格等于字符数；UI 侧限长为主）。
func MaxLen(field, v string, max int) Rule {
	return func() error {
		if len(v) > max {
			return fmt.Errorf("%s too long (max %d, got %d)", field, max, len(v))
		}
		return nil
	}
}

// Len 精确长度，通常用于 country(2) / currency(3)。
func Len(field, v string, exact int) Rule {
	return func() error {
		if v == "" {
			return nil // 空值由 Required 处理；这里只在非空时校验
		}
		if len(v) != exact {
			return fmt.Errorf("%s must be %d chars (got %d)", field, exact, len(v))
		}
		return nil
	}
}

// Email 粗粒度 email 合法性：有 @、有 .、不带空白。
// RFC 5322 太严；支付平台需要能用就行，严格合法性交给验真流程。
var emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

func Email(field, v string) Rule {
	return func() error {
		if v == "" {
			return nil
		}
		if !emailRe.MatchString(v) {
			return fmt.Errorf("%s is not a valid email", field)
		}
		return nil
	}
}

// CountryISO2 两位大写英文；空值跳过（由 Required 处理）。
var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

func CountryISO2(field, v string) Rule {
	return func() error {
		if v == "" {
			return nil
		}
		if !countryRe.MatchString(v) {
			return fmt.Errorf("%s must be ISO-3166 alpha-2 (got %q)", field, v)
		}
		return nil
	}
}

// CurrencyISO4217 三位大写英文。
var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

func CurrencyISO4217(field, v string) Rule {
	return func() error {
		if v == "" {
			return nil
		}
		if !currencyRe.MatchString(v) {
			return fmt.Errorf("%s must be ISO-4217 (got %q)", field, v)
		}
		return nil
	}
}

// HTTPURL 必须 http/https 且可 parse。
func HTTPURL(field, v string) Rule {
	return func() error {
		if v == "" {
			return nil
		}
		u, err := url.Parse(v)
		if err != nil {
			return fmt.Errorf("%s invalid URL: %v", field, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("%s must be http/https (got %q)", field, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("%s missing host", field)
		}
		return nil
	}
}

// OneOf 白名单。
func OneOf(field, v string, allowed ...string) Rule {
	return func() error {
		if v == "" {
			return nil
		}
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("%s = %q not one of %v", field, v, allowed)
	}
}

// Int64Range min <= v <= max；v 可为 0。
func Int64Range(field string, v, min, max int64) Rule {
	return func() error {
		if v < min || v > max {
			return fmt.Errorf("%s must be in [%d, %d] (got %d)", field, min, max, v)
		}
		return nil
	}
}

// Phone E.164 风格 + 容许常见分隔符。规则：
//   - 允许字符：+ 0-9 空格 - ( )
//   - 去除分隔符后纯数字长度 ∈ [7, 20]（E.164 上限 15 + 余量；下限 7 是国际惯例）
//   - 空串视作合法（字段是 optional；调用方应配合 Required 决定是否必填）
//
// 不强制特定国家格式：merchant 可能跨国，国家校验交给业务层。
var phoneAllowedRe = regexp.MustCompile(`^[+0-9\s\-()]+$`)

func Phone(field, v string) Rule {
	return func() error {
		if v == "" {
			return nil
		}
		if !phoneAllowedRe.MatchString(v) {
			return fmt.Errorf("%s contains invalid characters (allowed: + 0-9 space - ( ) )", field)
		}
		// 提取纯数字位长度
		var digits int
		for i := 0; i < len(v); i++ {
			if v[i] >= '0' && v[i] <= '9' {
				digits++
			}
		}
		if digits < 7 || digits > 20 {
			return fmt.Errorf("%s digit count must be in [7, 20] (got %d)", field, digits)
		}
		return nil
	}
}
