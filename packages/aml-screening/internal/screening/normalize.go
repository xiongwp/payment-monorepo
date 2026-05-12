// normalize.go — 名称 / 地址 / 国家代码标准化.
//
// AML 名称匹配的命中率主要看标准化质量, 不是匹配算法 (Jaro-Winkler 早就成熟).
// 这里实现工业界通用 6 步:
//
//   1) Unicode NFKD 去重音 (José → Jose)
//   2) lowercase
//   3) 公司后缀 stripping (Inc / Ltd / GmbH / 有限公司 …)
//   4) 标点 / 多空格折叠 (O'Neill → ONeill, 双空格 → 单)
//   5) 词序无关化 (按 alpha sort 排重)
//   6) 国家代码统一到 ISO 3166-1 alpha-2

package screening

import (
	"sort"
	"strings"
	"unicode"
)

// stripDiacritic 简易去重音 — 只覆盖 Latin Extended 常见字符.
// 生产替换 golang.org/x/text/unicode/norm.NFKD 更彻底, 但会引入额外依赖.
//
// 命中字符: ÁÀÂÄÃÅ → A, áàâäãå → a, Ç → C, ñ → n, ø → o, ß → ss …
var diacritic = map[rune]rune{
	'À': 'A', 'Á': 'A', 'Â': 'A', 'Ã': 'A', 'Ä': 'A', 'Å': 'A', 'Æ': 'A',
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a', 'æ': 'a',
	'Ç': 'C', 'ç': 'c',
	'È': 'E', 'É': 'E', 'Ê': 'E', 'Ë': 'E',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'Ì': 'I', 'Í': 'I', 'Î': 'I', 'Ï': 'I',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'Ñ': 'N', 'ñ': 'n',
	'Ò': 'O', 'Ó': 'O', 'Ô': 'O', 'Õ': 'O', 'Ö': 'O', 'Ø': 'O',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ø': 'o',
	'Ù': 'U', 'Ú': 'U', 'Û': 'U', 'Ü': 'U',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'Ý': 'Y', 'ý': 'y', 'ÿ': 'y',
	'Š': 'S', 'š': 's', 'Ž': 'Z', 'ž': 'z',
}

func foldDiacritics(s string) string {
	b := strings.Builder{}
	b.Grow(len(s))
	for _, r := range s {
		if v, ok := diacritic[r]; ok {
			b.WriteRune(v)
		} else if r == 'ß' {
			b.WriteString("ss")
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// 用 unicode pkg 兜底 — 避免 unused import.
var _ = unicode.IsLetter

// 公司后缀 — 所有命中后剥掉再比对
// 列表参考 OFAC SDN entity standard suffix list
var corporateSuffixes = []string{
	"corporation", "corp", "co", "company",
	"incorporated", "inc",
	"limited", "ltd", "ltda",
	"gmbh", "ag", "kg",
	"sa", "sas", "sarl",
	"bv", "nv",
	"plc",
	"llc", "lp", "llp",
	"pty", "pte",
	"oy", "oyj",
	"as", "asa",
	"trust", "foundation",
	"holdings",
	// 中文
	"有限公司", "股份有限公司", "有限责任公司", "集团", "控股",
	// 阿语 / 西语
	"sociedad", "anonima",
}

// 标点 → 空格 — '. , - / _ etc.
var punctReplacer = strings.NewReplacer(
	".", " ", ",", " ", "-", " ", "_", " ", "/", " ",
	"\\", " ", "(", " ", ")", " ", "[", " ", "]", " ",
	"{", " ", "}", " ", ":", " ", ";", " ", "*", " ",
	"+", " ", "=", " ", "@", " ", "#", " ", "$", " ",
	"%", " ", "^", " ", "&", " ", "?", " ", "!", " ",
	"<", " ", ">", " ", "|", " ", "~", " ", "`", " ",
	"'", "", "\"", "", // 单双引号直接吃掉, 不加空格 (O'Neill → ONeill)
)

// NormalizeName 把人名 / 公司名标准化用于匹配.
// 词序无关 ("John SMITH" 跟 "Smith John" 标准化结果一样).
func NormalizeName(s string) string {
	if s == "" {
		return ""
	}
	// 1) 简易去重音 (ASCII fold)
	out := foldDiacritics(s)
	// 2) lower
	out = strings.ToLower(out)
	// 3) 标点折叠
	out = punctReplacer.Replace(out)
	// 4) 空格折叠
	out = strings.Join(strings.Fields(out), " ")
	// 5) 公司后缀剥离 — 反复跑一次 (BMW AG Holdings → BMW)
	for changed := true; changed; {
		changed = false
		for _, suf := range corporateSuffixes {
			if strings.HasSuffix(out, " "+suf) {
				out = strings.TrimSpace(strings.TrimSuffix(out, " "+suf))
				changed = true
			}
		}
	}
	// 6) 词序无关化: split, sort, rejoin
	tokens := strings.Fields(out)
	sort.Strings(tokens)
	return strings.Join(tokens, " ")
}

// NormalizeAddress 地址标准化 - 主要去 noise word.
// 真生产需要 USPS / 邮政编码库, 这里给可读 baseline.
var addressNoise = []string{
	"street", "st", "road", "rd", "avenue", "ave", "boulevard", "blvd",
	"lane", "ln", "drive", "dr", "court", "ct", "place", "pl",
	"suite", "ste", "apt", "apartment", "unit", "floor", "fl",
	"north", "south", "east", "west", "n", "s", "e", "w",
	"号", "路", "街", "巷", "弄", "室", "栋", "幢",
}

func NormalizeAddress(s string) string {
	out := NormalizeName(s) // 复用前 4 步
	tokens := strings.Fields(out)
	kept := tokens[:0]
	skip := map[string]bool{}
	for _, w := range addressNoise {
		skip[w] = true
	}
	for _, t := range tokens {
		if skip[t] {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, " ")
}

// NormalizeCountry 国家代码 → ISO 3166-1 alpha-2.
// 不全, 列高频; 不在表的原样大写返回.
var countryMap = map[string]string{
	"china": "CN", "prc": "CN", "中国": "CN", "cn": "CN",
	"hong kong": "HK", "hk": "HK", "hkg": "HK",
	"taiwan": "TW", "tw": "TW", "twn": "TW",
	"united states": "US", "usa": "US", "us": "US", "america": "US",
	"united kingdom": "GB", "uk": "GB", "britain": "GB", "england": "GB", "gb": "GB", "gbr": "GB",
	"germany": "DE", "de": "DE", "deutschland": "DE", "deu": "DE",
	"france": "FR", "fr": "FR", "fra": "FR",
	"japan": "JP", "jp": "JP", "jpn": "JP",
	"russia": "RU", "russian federation": "RU", "ru": "RU", "rus": "RU",
	"iran": "IR", "ir": "IR", "irn": "IR",
	"north korea": "KP", "dprk": "KP", "kp": "KP",
	"cuba": "CU", "cu": "CU",
	"syria": "SY", "sy": "SY",
	"venezuela": "VE", "ve": "VE",
	"singapore": "SG", "sg": "SG", "sgp": "SG",
	"malaysia": "MY", "my": "MY", "mys": "MY",
}

func NormalizeCountry(s string) string {
	key := strings.ToLower(strings.TrimSpace(s))
	if v, ok := countryMap[key]; ok {
		return v
	}
	// fallback: 直接大写
	return strings.ToUpper(strings.TrimSpace(s))
}
