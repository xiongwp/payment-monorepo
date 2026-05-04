// Package money 跨服务共享的币种精度 + minor↔storage 换算 + 展示格式化。
//
// 约定：
//   - minor units = ISO 最小货币单位（USD cents / JPY yen / KWD fils）
//   - storage = minor_units × StorageScale（= 100，跨币种恒定）
//
// 例：
//
//	PHP 100.00 → minor_units=10000   → storage=1_000_000   → Display="₱100.00"
//	USD 100.34 → minor_units=10034   → storage=1_003_400   → Display="$100.34"
//	USD 100.346 → 已超 ISO 精度       → storage=1_003_460   → Display="$100.35"  (银行家舍入)
//	JPY 100    → minor_units=100     → storage=10_000      → Display="¥100"
//	KWD 1.000  → minor_units=1000    → storage=100_000     → Display="KD1.000"
//
// Display / FormatStorage 对超精度部分用 round-half-to-even（银行家舍入）；
// StorageToMinor 仍然严格，余数非零会报错——给账务路径用，避免吞掉零头。
package money

import (
	"fmt"
	"strconv"
)

// StorageScale accounting-system 内部 storage 相对 ISO minor units 的放大倍数。
// 跨币种恒定，目的是给未来高精度（加密货币小额手续费）场景留余量。
const StorageScale int64 = 100

// precisionMinor ISO 4217 最小货币单位相对主单位的小数位。覆盖所有现行
// ISO 4217 字母码（含贵金属/基金 X-codes）。新增/废止的币种维护这一份。
var precisionMinor = map[string]int{
	// ── 0 位（无小数）─────────────────────────────────────────
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0,
	"KMF": 0, "KRW": 0, "PYG": 0, "RWF": 0, "UGX": 0, "UYI": 0,
	"VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	"XAG": 0, "XAU": 0, "XPD": 0, "XPT": 0, // precious metals (per oz, no minor)
	"XBA": 0, "XBB": 0, "XBC": 0, "XBD": 0, // bond market units
	"XDR": 0, "XSU": 0, "XTS": 0, "XUA": 0, "XXX": 0, // SDR / Sucre / test / no-currency

	// ── 2 位（绝大多数）────────────────────────────────────────
	"AED": 2, "AFN": 2, "ALL": 2, "AMD": 2, "ANG": 2, "AOA": 2,
	"ARS": 2, "AUD": 2, "AWG": 2, "AZN": 2,
	"BAM": 2, "BBD": 2, "BDT": 2, "BGN": 2, "BMD": 2, "BND": 2,
	"BOB": 2, "BOV": 2, "BRL": 2, "BSD": 2, "BTN": 2, "BWP": 2,
	"BYN": 2, "BZD": 2,
	"CAD": 2, "CDF": 2, "CHE": 2, "CHF": 2, "CHW": 2, "CNY": 2,
	"COP": 2, "COU": 2, "CRC": 2, "CUC": 2, "CUP": 2, "CVE": 2,
	"CZK": 2,
	"DKK": 2, "DOP": 2, "DZD": 2,
	"EGP": 2, "ERN": 2, "ETB": 2, "EUR": 2,
	"FJD": 2, "FKP": 2,
	"GBP": 2, "GEL": 2, "GHS": 2, "GIP": 2, "GMD": 2, "GTQ": 2,
	"GYD": 2,
	"HKD": 2, "HNL": 2, "HTG": 2, "HUF": 2,
	"IDR": 2, "ILS": 2, "INR": 2, "IRR": 2,
	"JMD": 2,
	"KES": 2, "KGS": 2, "KHR": 2, "KPW": 2, "KYD": 2, "KZT": 2,
	"LAK": 2, "LBP": 2, "LKR": 2, "LRD": 2, "LSL": 2,
	"MAD": 2, "MDL": 2, "MGA": 2, "MKD": 2, "MMK": 2, "MNT": 2,
	"MOP": 2, "MRU": 2, "MUR": 2, "MVR": 2, "MWK": 2, "MXN": 2,
	"MXV": 2, "MYR": 2, "MZN": 2,
	"NAD": 2, "NGN": 2, "NIO": 2, "NOK": 2, "NPR": 2, "NZD": 2,
	"PAB": 2, "PEN": 2, "PGK": 2, "PHP": 2, "PKR": 2, "PLN": 2,
	"QAR": 2,
	"RON": 2, "RSD": 2, "RUB": 2,
	"SAR": 2, "SBD": 2, "SCR": 2, "SDG": 2, "SEK": 2, "SGD": 2,
	"SHP": 2, "SLE": 2, "SLL": 2, "SOS": 2, "SRD": 2, "SSP": 2,
	"STN": 2, "SVC": 2, "SYP": 2, "SZL": 2,
	"THB": 2, "TJS": 2, "TMT": 2, "TOP": 2, "TRY": 2, "TTD": 2,
	"TWD": 2, "TZS": 2,
	"UAH": 2, "USD": 2, "USN": 2, "UYU": 2, "UZS": 2,
	"VES": 2,
	"WST": 2,
	"XCD": 2,
	"YER": 2,
	"ZAR": 2, "ZMW": 2, "ZWG": 2, "ZWL": 2,

	// ── 3 位 ──────────────────────────────────────────────────
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3,
	"TND": 3,

	// ── 4 位 ──────────────────────────────────────────────────
	"CLF": 4, "UYW": 4,
}

// currencySymbol 给 Display 用的前缀。短符号紧贴金额（"$100"）；多字母无独占
// 符号的（"CHF"、"RM"、"Rp"…）也是紧贴。未登记的币种 Symbol() 回落到 ISO 码 +
// 空格，Display 会输出像 "BTN 100.00" 这样的串。
var currencySymbol = map[string]string{
	// ── 主流单字符符号 ────────────────────────────────────────
	"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥", "CNY": "¥",
	"PHP": "₱", "THB": "฿", "INR": "₹", "VND": "₫", "KRW": "₩",
	"TRY": "₺", "RUB": "₽", "UAH": "₴", "ILS": "₪", "KZT": "₸",
	"NGN": "₦", "GHS": "₵", "BDT": "৳", "MNT": "₮", "LAK": "₭",
	"KHR": "៛", "AFN": "؋", "AZN": "₼", "GEL": "₾", "PYG": "₲",
	"CRC": "₡",

	// ── "$" 系（区域加前缀避免和 USD 撞）──────────────────────
	"HKD": "HK$", "SGD": "S$", "AUD": "A$", "CAD": "C$", "TWD": "NT$",
	"NZD": "NZ$", "MXN": "Mex$", "BRL": "R$", "ARS": "AR$", "CLP": "CLP$",
	"COP": "COL$", "UYU": "$U", "CUP": "₱", "CUC": "CUC$", "DOP": "RD$",
	"BSD": "B$", "BBD": "Bds$", "BMD": "BD$", "BZD": "BZ$", "FJD": "FJ$",
	"GYD": "GY$", "JMD": "J$", "KYD": "KY$", "LRD": "L$", "NAD": "N$",
	"SBD": "SI$", "SRD": "Sr$", "TTD": "TT$", "TVD": "TV$",
	"XCD": "EC$", "ZWG": "ZWG$", "ZWL": "Z$",

	// ── "₨" / "Rs" 系 ────────────────────────────────────────
	"PKR": "₨", "LKR": "Rs", "NPR": "रू", "MUR": "₨", "SCR": "₨",

	// ── "kr" 系（北欧）────────────────────────────────────────
	"SEK": "kr", "NOK": "kr", "DKK": "kr", "ISK": "kr", "FOK": "kr",

	// ── 中欧/东欧多字母 ───────────────────────────────────────
	"PLN": "zł", "CZK": "Kč", "HUF": "Ft", "BGN": "лв", "RON": "lei",
	"RSD": "дин.", "MKD": "ден", "ALL": "L", "BAM": "KM", "MDL": "L",

	// ── 中东/北非阿语系 ───────────────────────────────────────
	"AED": "د.إ", "SAR": "﷼", "QAR": "﷼", "OMR": "ر.ع.", "BHD": "ب.د",
	"KWD": "د.ك", "JOD": "د.ا", "IQD": "ع.د", "LYD": "ل.د", "TND": "د.ت",
	"YER": "﷼", "EGP": "E£", "MAD": "د.م.", "DZD": "د.ج", "SDG": "ج.س.",
	"SYP": "ل.س", "LBP": "ل.ل", "IRR": "﷼",

	// ── 东南亚/南亚 ────────────────────────────────────────────
	"MYR": "RM", "IDR": "Rp", "MMK": "K", "BTN": "Nu.", "MVR": "Rf",
	"BND": "B$",

	// ── 非洲/中亚/其他 ────────────────────────────────────────
	"ZAR": "R", "BWP": "P", "ETB": "Br", "KES": "KSh", "TZS": "TSh",
	"UGX": "USh", "MWK": "MK", "ZMW": "K", "AOA": "Kz", "MZN": "MT",
	"GMD": "D", "SLL": "Le", "SLE": "Le", "STN": "Db", "ERN": "Nfk",
	"DJF": "Fdj", "KMF": "CF", "MGA": "Ar", "MRU": "UM", "RWF": "FRw",
	"BIF": "FBu", "GNF": "FG", "CDF": "FC", "XAF": "FCFA", "XOF": "CFA",
	"XPF": "₣", "TMT": "T", "TJS": "ЅМ", "UZS": "сўм", "AMD": "֏",
	"VUV": "VT", "WST": "T", "TOP": "T$", "PGK": "K", "FKP": "£",
	"GIP": "£", "SHP": "£", "SVC": "₡", "GTQ": "Q", "HNL": "L",
	"HTG": "G", "NIO": "C$", "PAB": "B/.", "PEN": "S/.", "BOB": "Bs.",
	"VES": "Bs.S", "ANG": "ƒ", "AWG": "ƒ", "KGS": "с", "BYN": "Br",
	"KPW": "₩",

	// ── 多字母无独占符号、保留 ISO 三字 ───────────────────────
	"CHF": "CHF", "LSL": "LSL", "SSP": "SSP", "SOS": "Sh.So.",
	"CVE": "Esc", "TJZ": "TJS", // safety dup
}

// IsSupported 币种是否已登记。
func IsSupported(code string) bool { _, ok := precisionMinor[code]; return ok }

// Precision 返回币种的 ISO 小数位。
func Precision(code string) (int, error) {
	p, ok := precisionMinor[code]
	if !ok {
		return 0, fmt.Errorf("money: unknown currency %q", code)
	}
	return p, nil
}

// MinorUnitsPerMajor 1 个主单位对应的最小单位数。USD=100, JPY=1, KWD=1000。
func MinorUnitsPerMajor(code string) (int64, error) {
	p, err := Precision(code)
	if err != nil {
		return 0, err
	}
	n := int64(1)
	for i := 0; i < p; i++ {
		n *= 10
	}
	return n, nil
}

// MinorToStorage minor_units → storage。跨币种都是 ×StorageScale。
func MinorToStorage(minorUnits int64, code string) (int64, error) {
	if !IsSupported(code) {
		return 0, fmt.Errorf("money: unknown currency %q", code)
	}
	return minorUnits * StorageScale, nil
}

// StorageToMinor 严格版：storage → minor_units。要求 storage % StorageScale == 0。
// 给账务/对账路径用，余数非零必须显式处理（拒绝静默吞掉零头）。
// 想直接 Display/Format 不在乎零头的，用 StorageToMinorBanker 或 Display。
func StorageToMinor(storage int64, code string) (int64, error) {
	if !IsSupported(code) {
		return 0, fmt.Errorf("money: unknown currency %q", code)
	}
	if storage%StorageScale != 0 {
		return 0, fmt.Errorf("money: storage %d not divisible by scale %d", storage, StorageScale)
	}
	return storage / StorageScale, nil
}

// StorageToMinorBanker storage → minor_units，超出 ISO 精度的余数按银行家舍入
// （round half to even）：>0.5 进位、<0.5 截掉、==0.5 朝偶数靠。
//
//	1003440 → 10034   (.4 < .5)
//	1003460 → 10035   (.6 > .5)
//	1003450 → 10034   (.5 → 偶数 4，不动)
//	1003550 → 10036   (.5 → 偶数 6，进位)
//	-1003460 → -10035 (符号对称)
func StorageToMinorBanker(storage int64, code string) (int64, error) {
	if !IsSupported(code) {
		return 0, fmt.Errorf("money: unknown currency %q", code)
	}
	return divHalfToEven(storage, StorageScale), nil
}

// divHalfToEven 整除 num/den 的银行家舍入版本。den 必须 > 0。
func divHalfToEven(num, den int64) int64 {
	sign := int64(1)
	if num < 0 {
		sign = -1
		num = -num
	}
	q := num / den
	r := num % den
	twoR := r * 2
	switch {
	case twoR > den:
		q++
	case twoR == den:
		// 正好 0.5：朝偶数靠
		if q%2 != 0 {
			q++
		}
	}
	return sign * q
}

// Symbol 返回币种用于展示的前缀。未登记的币种回落到 "<CODE> "（带尾随空格），
// 这样 Symbol(code)+amount 永远是合法可读字符串。
func Symbol(code string) string {
	if s, ok := currencySymbol[code]; ok {
		return s
	}
	return code + " "
}

// FormatMinor minor_units → 主单位字符串，按 ISO 小数位补零。
//
//	USD 10034 → "100.34"
//	JPY 100   → "100"
//	KWD 1500  → "1.500"
//	USD -25   → "-0.25"
func FormatMinor(minorUnits int64, code string) (string, error) {
	p, err := Precision(code)
	if err != nil {
		return "", err
	}
	sign := ""
	if minorUnits < 0 {
		sign = "-"
		minorUnits = -minorUnits
	}
	if p == 0 {
		return sign + strconv.FormatInt(minorUnits, 10), nil
	}
	div := int64(1)
	for i := 0; i < p; i++ {
		div *= 10
	}
	major := minorUnits / div
	frac := minorUnits % div
	return fmt.Sprintf("%s%d.%0*d", sign, major, p, frac), nil
}

// FormatStorage storage → 主单位字符串。超精度按银行家舍入到 ISO minor。
//
//	USD 1_003_400 → "100.34"
//	USD 1_003_460 → "100.35"
func FormatStorage(storage int64, code string) (string, error) {
	m, err := StorageToMinorBanker(storage, code)
	if err != nil {
		return "", err
	}
	return FormatMinor(m, code)
}

// RoundToStorage storage → 主单位字符串。超精度按银行家舍入到 ISO minor。
//
//	USD 1_003_400 → "1003400"
//	USD 1_003_460 → "1003500"
func RoundToStorage(storage int64, code string) (int64, error) {
	m, err := StorageToMinorBanker(storage, code)
	if err != nil {
		return 0, err
	}
	return m * StorageScale, nil
}

// Display storage → 带币种符号的人类可读金额。给 ledger/admin UI/收据用。
//
//	USD 1_003_400 → "$100.34"
//	USD 1_003_460 → "$100.35"   (banker)
//	USD 1_003_450 → "$100.34"   (banker：偶数侧)
//	USD 1_003_550 → "$100.36"   (banker：偶数侧)
//	JPY 10_000    → "¥100"
//	KWD 150_000   → "KD1.000"
//	未登记币种    → "XYZ 100.00"
func Display(storage int64, code string) (string, error) {
	s, err := FormatStorage(storage, code)
	if err != nil {
		return "", err
	}
	return Symbol(code) + s, nil
}
