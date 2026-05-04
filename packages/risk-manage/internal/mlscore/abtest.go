// abtest.go —— Champion-Challenger 显著性检验
//
// 当前 C-C 框架只是把 challenger 跑起来 + 把 score 落 audit；运营升级模型时
// 没有 quantitative reasoning："challenger AUC 高 0.02 是真好还是抖动？"
//
// 本文件提供：
//   1. ABTracker —— 在 SideEffect 回调里记录 (decision_id, champion_score,
//      challenger_score)。ring buffer 限制内存（默认 4096 条）。
//   2. ABReport(fbRec) —— 跟 feedback.Recorder 反查 outcome label，对每个
//      challenger 算：
//        - sample / labeled_total
//        - champion_auc / challenger_auc
//        - 95% bootstrap CI on (challenger_auc - champion_auc)
//        - recommendation: promote / hold / drop
//   3. /admin/mlscore/abtest endpoint 暴露给 admin-web。
//
// 显著性的判断：bootstrap 1000 次重采样 (decision_id, label, both_scores)
// 三元组，每次重新算 AUC 差。CI 全 > 0 = challenger 显著更好（promote）；
// 全 < 0 = 显著更差（drop）；跨 0 = 数据还不够 / 差异不显著（hold）。
package mlscore

import (
	"math/rand"
	"sort"
	"sync"
)

// ABTracker 记录 (decision_id, champion_score, challenger_scores...) 三元组。
// 简单 ring buffer + map 索引，给 ABReport 反查用。
type ABTracker struct {
	mu        sync.RWMutex
	cap       int
	entries   []abEntry // ring buffer
	cursor    int
	full      bool
	byID      map[string]int // decision_id → entries 下标
}

type abEntry struct {
	DecisionID       string
	ChampionScore    float64
	ChallengerScores map[string]float64 // name → score
}

// NewABTracker cap 上限的 ring；cap <= 0 → 4096。
func NewABTracker(cap int) *ABTracker {
	if cap <= 0 {
		cap = 4096
	}
	return &ABTracker{
		cap:     cap,
		entries: make([]abEntry, cap),
		byID:    make(map[string]int),
	}
}

// Record 写入一条三元组。decisionID="" 时跳过（无法跟 outcome 关联）。
func (t *ABTracker) Record(decisionID string, championScore float64, challengers []NamedResult) {
	if t == nil || decisionID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// 旧条目从 byID 清掉（ring 覆写）
	if old := t.entries[t.cursor]; old.DecisionID != "" {
		if existing, ok := t.byID[old.DecisionID]; ok && existing == t.cursor {
			delete(t.byID, old.DecisionID)
		}
	}
	chal := make(map[string]float64, len(challengers))
	for _, c := range challengers {
		if c.Error == "" {
			chal[c.Name] = c.Score
		}
	}
	t.entries[t.cursor] = abEntry{
		DecisionID:       decisionID,
		ChampionScore:    championScore,
		ChallengerScores: chal,
	}
	t.byID[decisionID] = t.cursor
	t.cursor = (t.cursor + 1) % t.cap
	if t.cursor == 0 {
		t.full = true
	}
}

// Snapshot 浅拷贝当前所有条目（给 Report / test 用）。
func (t *ABTracker) Snapshot() []abEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	size := t.cursor
	if t.full {
		size = t.cap
	}
	out := make([]abEntry, 0, size)
	for i := 0; i < size; i++ {
		idx := (t.cursor - 1 - i + t.cap) % t.cap
		if t.entries[idx].DecisionID != "" {
			out = append(out, t.entries[idx])
		}
	}
	return out
}

// ChallengerReport 单个 challenger 的检验结果。
type ChallengerReport struct {
	Name              string  `json:"name"`
	LabeledSamples    int     `json:"labeled_samples"`
	ChampionAUC       float64 `json:"champion_auc"`
	ChallengerAUC     float64 `json:"challenger_auc"`
	AUCDiff           float64 `json:"auc_diff"` // challenger - champion
	CILow             float64 `json:"ci_low"`   // 95% bootstrap CI on diff
	CIHigh            float64 `json:"ci_high"`
	Recommendation    string  `json:"recommendation"` // promote / hold / drop
	RecommendReason   string  `json:"recommend_reason"`
}

// Report 跑一次 A/B 检验。需要 caller 提供 outcome 反查回调
// （feedback.Recorder 不能直接 import 因为会循环依赖 — 上层 wrap）。
//
// minLabeled：每个 challenger 至少有 N 条 labeled 样本才输出；< minLabeled
// 直接跳过避免噪音。默认 30（保证 AUC SE < 0.1 量级）。
//
// bootstrapN：bootstrap 重采样次数，缺省 1000。<= 0 时跳过 CI（只输出
// 点估计 AUC）。
func (t *ABTracker) Report(getOutcome func(decisionID string) (isFraud bool, ok bool), minLabeled, bootstrapN int) []ChallengerReport {
	if t == nil {
		return nil
	}
	if minLabeled <= 0 {
		minLabeled = 30
	}
	if bootstrapN < 0 {
		bootstrapN = 0
	}
	entries := t.Snapshot()

	// 收集每个 challenger 的 (champion, challenger, label) 三元组。
	type triple struct {
		champ, chal float64
		label       float64 // 1=fraud, 0=legit
	}
	byChal := make(map[string][]triple)

	for _, e := range entries {
		isFraud, ok := getOutcome(e.DecisionID)
		if !ok {
			continue
		}
		label := 0.0
		if isFraud {
			label = 1.0
		}
		for name, chalScore := range e.ChallengerScores {
			byChal[name] = append(byChal[name], triple{
				champ: e.ChampionScore, chal: chalScore, label: label,
			})
		}
	}

	out := make([]ChallengerReport, 0, len(byChal))
	for name, ts := range byChal {
		if len(ts) < minLabeled {
			continue
		}
		// 转成 tripleAlias slice 给 computeAUC / bootstrap
		tas := make([]tripleAlias, len(ts))
		for i, t := range ts {
			tas[i] = tripleAlias(t)
		}
		champAUC := computeAUC(tas, true)
		chalAUC := computeAUC(tas, false)
		rep := ChallengerReport{
			Name:           name,
			LabeledSamples: len(ts),
			ChampionAUC:    champAUC,
			ChallengerAUC:  chalAUC,
			AUCDiff:        chalAUC - champAUC,
		}
		if bootstrapN > 0 {
			lo, hi := bootstrapAUCDiff(tas, bootstrapN)
			rep.CILow = lo
			rep.CIHigh = hi
		}
		// 推荐
		switch {
		case bootstrapN <= 0:
			rep.Recommendation = "hold"
			rep.RecommendReason = "bootstrap CI 未跑，仅点估计；建议样本 >= 500 后再判"
		case rep.CILow > 0:
			rep.Recommendation = "promote"
			rep.RecommendReason = "challenger AUC 95% CI 全 > 0，显著优于 champion"
		case rep.CIHigh < 0:
			rep.Recommendation = "drop"
			rep.RecommendReason = "challenger AUC 95% CI 全 < 0，显著差于 champion"
		default:
			rep.Recommendation = "hold"
			rep.RecommendReason = "AUC 差跨 0 — 数据不够或差异不显著"
		}
		out = append(out, rep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AUCDiff > out[j].AUCDiff })
	return out
}

// computeAUC Mann-Whitney U 法。useChamp=true 用 triple.champ；false 用 chal。
func computeAUC(ts []tripleAlias, useChamp bool) float64 {
	type pair struct {
		score float64
		label float64
	}
	pairs := make([]pair, len(ts))
	for i, t := range ts {
		s := t.chal
		if useChamp {
			s = t.champ
		}
		pairs[i] = pair{s, t.label}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].score < pairs[j].score })
	var sumRank float64
	var nPos, nNeg int
	for i, p := range pairs {
		if p.label == 1 {
			sumRank += float64(i + 1)
			nPos++
		} else {
			nNeg++
		}
	}
	if nPos == 0 || nNeg == 0 {
		return 0.5
	}
	return (sumRank - float64(nPos*(nPos+1)/2)) / float64(nPos*nNeg)
}

// triple alias to keep computeAUC's signature decoupled from the local
// `triple` declared inside Report.
type tripleAlias = struct {
	champ, chal float64
	label       float64
}

// bootstrapAUCDiff 重采样 N 次 ts，每次有放回采样 len(ts) 条，算 AUC 差。
// 返回 95% CI (2.5% / 97.5% 分位数)。
func bootstrapAUCDiff(ts []tripleAlias, N int) (lo, hi float64) {
	if len(ts) < 5 || N <= 0 {
		return 0, 0
	}
	rng := rand.New(rand.NewSource(42)) // 固定 seed 让 admin endpoint 可复现
	diffs := make([]float64, 0, N)
	resampled := make([]tripleAlias, len(ts))
	for k := 0; k < N; k++ {
		for i := range resampled {
			resampled[i] = ts[rng.Intn(len(ts))]
		}
		champ := computeAUC(resampled, true)
		chal := computeAUC(resampled, false)
		diffs = append(diffs, chal-champ)
	}
	sort.Float64s(diffs)
	lo = diffs[int(float64(N)*0.025)]
	hi = diffs[int(float64(N)*0.975)]
	return lo, hi
}
