// Fuzzy 名字匹配 —— Levenshtein 编辑距离 + token-level fuzzy。
//
// 设计：
//   - Levenshtein O(m*n) DP，两行滚动数组 O(min(m,n)) 空间
//   - FuzzyMatch(query, target, maxDist) → bool
//   - SimilarityRatio(a, b) → [0,1] 越接近 1 越像
//   - 在 sanction 包内新增 *MemService.CheckWithFuzzy 方法，
//     在原 Check 不命中时做 token-level fuzzy fallback。
//
// 不修改 Service 接口（只在 MemService 上加方法）；不修改 Match struct
// （新增 FuzzyMatch 结果类型把 fuzzy 标志携带回调用方）。
//
// 复杂度：
//   - Levenshtein(a,b): O(len(a)*len(b)) 时间, O(min) 空间
//   - CheckWithFuzzy: O(N_candidates * T_query * len_token^2)
//     N_candidates 由 first-token bucket 限定（通常 < 100）

package sanction

import (
	"context"
)

// Levenshtein 经典编辑距离：插入 / 删除 / 替换各 +1。
// 两行滚动数组实现，空间 O(min(len(a),len(b))+1)。
func Levenshtein(a, b string) int {
	ra := []rune(a)
	rb := []rune(b)
	// 让 b 是较短的那个，减少空间
	if len(ra) < len(rb) {
		ra, rb = rb, ra
	}
	la, lb := len(ra), len(rb)
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			// min(insert, delete, substitute)
			ins := curr[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			m := ins
			if del < m {
				m = del
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

// FuzzyMatch 先 normalize 再判 Levenshtein 距离是否 <= maxDist。
func FuzzyMatch(query, target string, maxDist int) bool {
	q := normalizeName(query)
	t := normalizeName(target)
	if q == "" || t == "" {
		return false
	}
	if q == t {
		return true
	}
	// 长度差超过 maxDist 直接 false（距离的下界 = abs(len 差））
	dq, dt := len(q), len(t)
	diff := dq - dt
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDist {
		return false
	}
	return Levenshtein(q, t) <= maxDist
}

// SimilarityRatio 1 - dist/maxLen，全相同返 1，完全不同返 0。
func SimilarityRatio(a, b string) float64 {
	ra := []rune(normalizeName(a))
	rb := []rune(normalizeName(b))
	maxLen := len(ra)
	if len(rb) > maxLen {
		maxLen = len(rb)
	}
	if maxLen == 0 {
		return 1.0
	}
	d := Levenshtein(string(ra), string(rb))
	return 1.0 - float64(d)/float64(maxLen)
}

// FuzzyResult 包装现有 *Match 并标记是不是 fuzzy 命中（运营 UI 据此区分）。
type FuzzyResult struct {
	*Match
	Fuzzy bool
	// DOBLevel: 若上游做 DOB 二级匹配，可填 "exact"|"year"|"unknown"。
	DOBLevel string
}

// CheckWithFuzzy 在原 Check（exact + token bag）不命中时，做 token-level
// Levenshtein fuzzy fallback。命中即返。distance <= maxTokenDist（默认 2）
// 视为相似。country 过滤跟原 Check 一致。
//
// 不修改原 Check；调用方有需要时改调本方法。
func (s *MemService) CheckWithFuzzy(ctx context.Context, fullName, country string, maxTokenDist int) *FuzzyResult {
	if maxTokenDist < 0 {
		maxTokenDist = 2
	}
	if m := s.Check(ctx, fullName, country); m != nil {
		return &FuzzyResult{Match: m, Fuzzy: false}
	}
	n := normalizeName(fullName)
	if n == "" {
		return nil
	}
	qTokens := tokenize(n)
	if len(qTokens) == 0 {
		return nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// 把候选集合限到"至少有一个 query token 的 fuzzy near-token 出现过"。
	// 简化：直接对 all 全扫但带 country 过滤。N≈10万、tokens 小，仍可控；
	// 生产可换 bigram index 加速。
	hits := make([]*Entry, 0, 4)
	seen := make(map[*Entry]struct{})
	for _, e := range s.all {
		if _, ok := seen[e]; ok {
			continue
		}
		if country != "" && e.Country != "" && !equalFoldStr(country, e.Country) {
			continue
		}
		if entryFuzzyMatches(e, qTokens, maxTokenDist) {
			seen[e] = struct{}{}
			hits = append(hits, e)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	return &FuzzyResult{
		Match: &Match{Hits: hits, NormalizedName: n},
		Fuzzy: true,
	}
}

// entryFuzzyMatches: 对每个 query token，entry 至少有一个 token 距离 <= maxDist。
// 长度过滤 + 早退；O(T_q * T_e * len^2)，T_e 通常 < 10。
func entryFuzzyMatches(e *Entry, qTokens []string, maxDist int) bool {
	eTokens := mergeNameTokens(e)
	if len(eTokens) == 0 {
		return false
	}
	for _, q := range qTokens {
		ok := false
		for _, et := range eTokens {
			diff := len(q) - len(et)
			if diff < 0 {
				diff = -diff
			}
			if diff > maxDist {
				continue
			}
			if Levenshtein(q, et) <= maxDist {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// equalFoldStr ASCII case-insensitive 比较。country 都是 ISO-2 / ISO-3 大写或小写。
// 比走 strings.EqualFold 略快且不需要额外 import。
func equalFoldStr(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}
