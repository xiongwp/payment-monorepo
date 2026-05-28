// DOB（出生日期）二级匹配。fuzzy name 命中后用 DOB 收窄置信度：
//   - exact   : 完整 YYYY-MM-DD 一致
//   - year    : 只能匹配到年（OFAC 老条目常只给年份）
//   - unknown : 其中一边没填 DOB；不做降权
//
// 名字 fuzzy 命中 + DOB 不一致 → 强烈不是同一人 → 调用方可降 confidence。

package sanction

import (
	"strings"
	"time"
)

// dobFormats 兼容 OFAC / EU / 自填 CSV 的常见格式。
var dobFormats = []string{
	"2006-01-02",
	"02 Jan 2006",
	"2 Jan 2006",
	"2006/01/02",
	"01/02/2006", // 美式
	"02-01-2006", // 欧式
	"2006",       // 仅年
}

// ParseDOB 解析常见 DOB 字符串。返 (time, err)。
// 注意只精确到年的输入返回的 time.Month/Day 都是 1。
func ParseDOB(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errEmptyDOB
	}
	var lastErr error
	for _, f := range dobFormats {
		t, err := time.Parse(f, s)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return time.Time{}, lastErr
}

type stringError string

func (e stringError) Error() string { return string(e) }

var errEmptyDOB = stringError("dob: empty")

// MatchDOB 比较 query DOB 与 target DOB（target 来自 Entry.BirthDate）。
// 返回 (matched, level)。level ∈ {"exact","year","unknown"}。
// 任一边缺值 → 返 (true, "unknown")（不降权但也不加权）。
func MatchDOB(query, target string) (bool, string) {
	qs := strings.TrimSpace(query)
	ts := strings.TrimSpace(target)
	if qs == "" || ts == "" {
		return true, "unknown"
	}
	qt, qerr := ParseDOB(qs)
	tt, terr := ParseDOB(ts)
	if qerr != nil || terr != nil {
		return true, "unknown"
	}
	if qt.Year() == tt.Year() &&
		qt.Month() == tt.Month() &&
		qt.Day() == tt.Day() {
		return true, "exact"
	}
	if qt.Year() == tt.Year() {
		// 年同但月/日不同：仍可能是同人（数据源精度差）
		return true, "year"
	}
	return false, ""
}
