// Package money: ISO 4217 货币代码映射。
//
// account_id layout 用 3 位数字编码货币（payment-util/shadow/identity.go 的
// EncodeAccountID 接受 currency int 参数），需要把业务侧的字符串 "PHP" / "USD"
// 转成 ISO 4217 数字码。
//
// 全球活跃货币约 180 种，ISO 4217 数字码上限 999。本表只列东南亚 / 美元区 /
// 主要法币 + 加密；新增按字母序补在 currencyByCode 里。
package money

import "fmt"

// currencyByCode: ISO 4217 三字母代码 → 三位数字代码。
//
// 来源：https://www.iso.org/iso-4217-currency-codes.html
// 完整 list 见 ISO；这里只列项目实际可能用到的。
var currencyByCode = map[string]int{
	// 东南亚（菲律宾 / 印尼 / 越南 / 泰国 / 马来 / 新加坡）
	"PHP": 608, // Philippine Peso
	"IDR": 360, // Indonesian Rupiah
	"VND": 704, // Vietnamese Dong
	"THB": 764, // Thai Baht
	"MYR": 458, // Malaysian Ringgit
	"SGD": 702, // Singapore Dollar

	// 主要法币
	"USD": 840, // US Dollar
	"EUR": 978, // Euro
	"GBP": 826, // British Pound
	"JPY": 392, // Japanese Yen
	"CNY": 156, // Chinese Yuan
	"HKD": 344, // Hong Kong Dollar
	"TWD": 901, // Taiwan Dollar
	"KRW": 410, // South Korean Won
	"AUD": 36,  // Australian Dollar
	"CAD": 124, // Canadian Dollar
	"CHF": 756, // Swiss Franc
	"INR": 356, // Indian Rupee

	// 拉丁美洲
	"BRL": 986, // Brazilian Real
	"MXN": 484, // Mexican Peso
	"ARS": 32,  // Argentine Peso

	// 中东
	"AED": 784, // UAE Dirham
	"SAR": 682, // Saudi Riyal
}

// codeByNumeric: 反向查表（数字码 → 字母码），用于日志 / debug。
var codeByNumeric = func() map[int]string {
	m := make(map[int]string, len(currencyByCode))
	for code, num := range currencyByCode {
		m[num] = code
	}
	return m
}()

// NumericCode 返回 ISO 4217 三位数字代码。
//   - 输入 "PHP" → 608
//   - 输入 "USD" → 840
//   - 输入未注册的代码 → error
//
// 调用方在 EncodeAccountID 之前必须先调用本函数；layout 上 currency 字段是数字
// 而非字符串。新增货币时直接在 currencyByCode 里加一行即可，layout 不动（3 位
// 数字段最大 999，ISO 4217 当前最大约 985，留有充足扩展余量）。
func NumericCode(code string) (int, error) {
	if n, ok := currencyByCode[code]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("unknown currency code %q (extend payment-util/money/iso4217.go)", code)
}

// MustNumericCode 同 NumericCode 但 panic on error。
// 仅用于"不可能失败"的场景（如 init.sql 生成器对硬编码 PHP 转换）。
func MustNumericCode(code string) int {
	n, err := NumericCode(code)
	if err != nil {
		panic(err)
	}
	return n
}

// AlphaCode 返回三字母代码（用于日志展示 decode 后的 currency 数字码）。
//   - 608 → "PHP"
//   - 840 → "USD"
//   - 未注册的数字码 → 返回 fmt.Sprintf 的字符串形式（不报错，便于 log）
func AlphaCode(num int) string {
	if s, ok := codeByNumeric[num]; ok {
		return s
	}
	return fmt.Sprintf("CCY-%d", num)
}
