// Package currency 提供 ISO 4217 货币精度注册表及金额存储转换工具。
//
// 存储约定：
//
//	数据库和 Redis 中的金额统一以 BIGINT 存储，单位为「ISO 最小货币单位 × 100」。
//	示例（USD，精度 2 位）：$3.42 = 342 美分 × 100 = 34200
//	示例（JPY，精度 0 位）：¥342 = 342 日元 × 100 = 34200
//	示例（KWD，精度 3 位）：3.422 KWD = 3422 fils × 100 = 342200
//
// 存储系数 = 10^precision × 100
//
// API 层（gRPC）接受/返回的金额使用 ISO 精度的十进制字符串（如 "3.42"），
// 与存储值之间通过 ToStorage / FromStorage 互转。
package currency

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// precisionMap 记录每个 ISO 4217 货币代码的小数位数（最小货币单位的幂次）。
// 例如 USD=2 表示最小单位为分（0.01 USD）。
var precisionMap = map[string]int{
	"PHP": 2, // 菲律宾比索 Philippine Peso
	"USD": 2, // 美元 US Dollar
	"EUR": 2, // 欧元 Euro
	"GBP": 2, // 英镑 British Pound Sterling
	"HKD": 2, // 港元 Hong Kong Dollar
	"SGD": 2, // 新加坡元 Singapore Dollar
	"AUD": 2, // 澳大利亚元 Australian Dollar
	"CAD": 2, // 加拿大元 Canadian Dollar
	"CHF": 2, // 瑞士法郎 Swiss Franc
	"CNY": 2, // 人民币 Chinese Yuan
	"MYR": 2, // 马来西亚林吉特 Malaysian Ringgit
	"THB": 2, // 泰铢 Thai Baht
	"INR": 2, // 印度卢比 Indian Rupee
	"TWD": 2, // 新台币 New Taiwan Dollar
	"VND": 0, // 越南盾 Vietnamese Dong
	"IDR": 0, // 印度尼西亚盾 Indonesian Rupiah
	"JPY": 0, // 日元 Japanese Yen
	"KRW": 0, // 韩元 South Korean Won
	"KWD": 3, // 科威特第纳尔 Kuwaiti Dinar
	"BHD": 3, // 巴林第纳尔 Bahraini Dinar
	"OMR": 3, // 阿曼里亚尔 Omani Rial
}

// storageFactor 缓存每种货币的存储系数 = 10^precision × 100。
var storageFactor map[string]int64

func init() {
	storageFactor = make(map[string]int64, len(precisionMap))
	factor := int64(1)
	for code, prec := range precisionMap {
		factor = 1
		for i := 0; i < prec; i++ {
			factor *= 10
		}
		storageFactor[code] = factor * 100
	}
}

// Precision 返回货币的小数位数（ISO 4217 最小单位的幂次）。
// 例如 USD=2，JPY=0，KWD=3。
func Precision(code string) (int, error) {
	p, ok := precisionMap[code]
	if !ok {
		return 0, fmt.Errorf("unknown currency: %s", code)
	}
	return p, nil
}

// StorageFactor 返回货币的存储系数 = 10^precision × 100。
func StorageFactor(code string) (int64, error) {
	f, ok := storageFactor[code]
	if !ok {
		return 0, fmt.Errorf("unknown currency: %s", code)
	}
	return f, nil
}

// IsSupported 判断货币代码是否受支持。
func IsSupported(code string) bool {
	_, ok := precisionMap[code]
	return ok
}

// ToStorage 将人类可读的货币金额（如 "3.42"）转换为 BIGINT 存储值。
// 转换规则：存储值 = round(amount × storageFactor)
// 若金额含超过 (precision+2) 位小数，截断多余精度后四舍五入。
func ToStorage(amount decimal.Decimal, code string) (int64, error) {
	f, ok := storageFactor[code]
	if !ok {
		return 0, fmt.Errorf("unknown currency: %s", code)
	}
	stored := amount.Mul(decimal.NewFromInt(f)).Round(0)
	return stored.IntPart(), nil
}

// ToStorageFromMinor 把 ISO 最小货币单位（cents / 分 / 元）转成存储值。
// 内部 storage 精度比 ISO minor 多 2 位（storageFactor = 10^precision × 100），
// 所以换算永远是 minorUnits × 100，不随币种变化。
func ToStorageFromMinor(minorUnits int64, code string) (int64, error) {
	if _, ok := precisionMap[code]; !ok {
		return 0, fmt.Errorf("unknown currency: %s", code)
	}
	return minorUnits * 100, nil
}

// FromStorage 将 BIGINT 存储值还原为 ISO 精度的 decimal.Decimal。
// 返回值的小数位数等于该货币的 precision（例如 USD 返回 2 位小数）。
func FromStorage(stored int64, code string) (decimal.Decimal, error) {
	f, ok := storageFactor[code]
	if !ok {
		return decimal.Zero, fmt.Errorf("unknown currency: %s", code)
	}
	prec, _ := precisionMap[code]
	result := decimal.NewFromInt(stored).
		Div(decimal.NewFromInt(f)).
		Round(int32(prec)) //nolint:gosec
	return result, nil
}

// FormatAmount 将存储值格式化为 ISO 精度的货币字符串（如 "3.42"）。
// 用于 API 响应。
func FormatAmount(stored int64, code string) (string, error) {
	d, err := FromStorage(stored, code)
	if err != nil {
		return "", err
	}
	return d.StringFixed(int32(precisionMap[code])), nil //nolint:gosec
}
