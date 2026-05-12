// jaro.go — Jaro-Winkler 相似度.
//
// AML 业内标准匹配算法: Jaro 计算两串字符重叠 + 转置, Winkler 加成前缀匹配权重.
// 实现照 Cohen et al. (2003) "A Comparison of String Distance Metrics for Name-Matching Tasks".
//
// 输出 [0,1] — 1.0 完全相同, 0 完全不同.
// 业界 cutoff:
//   > 0.95 — 强匹配 (几乎必命中)
//   > 0.85 — 中匹配 (转人工)
//   > 0.75 — 弱匹配 (低风险场景才告警)

package screening

// JaroWinkler 返回 [0,1] 相似度.
// 调用前先 NormalizeName.
func JaroWinkler(a, b string) float64 {
	if a == b {
		return 1.0
	}
	if a == "" || b == "" {
		return 0
	}
	j := jaro(a, b)
	if j < 0.7 {
		// Winkler 只对中高分加成 - 低分维持
		return j
	}
	// 前缀匹配 (最长 4 字符)
	prefix := 0
	maxPrefix := 4
	for i := 0; i < len(a) && i < len(b) && i < maxPrefix; i++ {
		if a[i] != b[i] {
			break
		}
		prefix++
	}
	return j + float64(prefix)*0.1*(1.0-j)
}

func jaro(a, b string) float64 {
	la, lb := len(a), len(b)
	if la == 0 && lb == 0 {
		return 1.0
	}
	if la == 0 || lb == 0 {
		return 0
	}
	// 匹配窗口: max(|a|,|b|)/2 - 1
	mw := max(la, lb)/2 - 1
	if mw < 0 {
		mw = 0
	}
	aMatch := make([]bool, la)
	bMatch := make([]bool, lb)
	matches := 0
	for i := 0; i < la; i++ {
		lo := max(0, i-mw)
		hi := min(lb-1, i+mw)
		for j := lo; j <= hi; j++ {
			if bMatch[j] {
				continue
			}
			if a[i] != b[j] {
				continue
			}
			aMatch[i] = true
			bMatch[j] = true
			matches++
			break
		}
	}
	if matches == 0 {
		return 0
	}
	// 转置数 / 2
	t := 0
	k := 0
	for i := 0; i < la; i++ {
		if !aMatch[i] {
			continue
		}
		for !bMatch[k] {
			k++
		}
		if a[i] != b[k] {
			t++
		}
		k++
	}
	transposes := t / 2
	m := float64(matches)
	return (m/float64(la) + m/float64(lb) + (m-float64(transposes))/m) / 3.0
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
