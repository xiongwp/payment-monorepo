// matcher.go — 名单匹配主入口.
//
// 输入: ScreenRequest + 候选名单条目 (用 first-letter / nationality 等 cheap filter 先粗筛后传入)
// 输出: 排序的 HitInfo 数组 (confidence 高到低)
//
// confidence 加权 (0-100):
//   primary_name 强匹配 (>0.92) → +60
//   alias 强匹配                → +55 (略低于 primary)
//   dob 完全匹配               → +20
//   nationality 命中            → +10
//   id_number hash 命中          → +90 (近乎确定, 单字段满分)
//   address strong              → +10
//
// 阈值 (默认):
//   >= 90 → block
//   >= 70 → review
//   <  70 → pass

package screening

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"reconcile-system/packages/aml-screening/internal/domain"
)

// Config 匹配配置 — 可热更
type Config struct {
	BlockThreshold  int `json:"block_threshold"`   // 默认 90
	ReviewThreshold int `json:"review_threshold"`  // 默认 70
	MaxHitsReturned int `json:"max_hits_returned"` // 默认 10
}

func DefaultConfig() Config {
	return Config{BlockThreshold: 90, ReviewThreshold: 70, MaxHitsReturned: 10}
}

// Match 给一个请求 + 候选列表跑匹配, 返回排序的命中.
//
// candidates 一般已经过 candidate-store 的索引粗筛 (first-letter, source, country):
// 全表 70 万条 → 几百条候选 → matcher fine-grained.
func Match(req domain.ScreenRequest, candidates []domain.ListEntry, cfg Config) []domain.HitInfo {
	normReq := normalizeReq(req)
	hits := make([]domain.HitInfo, 0, len(candidates))

	for _, c := range candidates {
		h := scoreOne(normReq, c)
		if h.Confidence == 0 {
			continue
		}
		hits = append(hits, h)
	}

	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].Confidence > hits[j].Confidence
	})

	if len(hits) > cfg.MaxHitsReturned {
		hits = hits[:cfg.MaxHitsReturned]
	}
	return hits
}

// Decide hits → action (pass/review/block)
func Decide(hits []domain.HitInfo, cfg Config) (domain.Action, int) {
	if len(hits) == 0 {
		return domain.ActionPass, 0
	}
	top := hits[0].Confidence
	switch {
	case top >= cfg.BlockThreshold:
		return domain.ActionBlock, top
	case top >= cfg.ReviewThreshold:
		return domain.ActionReview, top
	default:
		return domain.ActionPass, top
	}
}

// ── internal ──────────────────────────────────────────────

type normalizedReq struct {
	nameN       string
	dob         string
	nationality string
	idHash      string
	idType      string
	idCountry   string
	addrsN      []string
}

func normalizeReq(req domain.ScreenRequest) normalizedReq {
	out := normalizedReq{
		nameN:       NormalizeName(req.Name),
		dob:         strings.TrimSpace(req.DOB),
		nationality: NormalizeCountry(req.Nationality),
		idType:      strings.ToUpper(strings.TrimSpace(req.IDType)),
		idCountry:   NormalizeCountry(req.IDCountry),
	}
	if req.IDNumber != "" {
		// 调用方应已传 hash; 如传入是 64hex 不再二哈, 否则做一次 sha256
		raw := strings.TrimSpace(req.IDNumber)
		if isHex64(raw) {
			out.idHash = raw[:16]
		} else {
			sum := sha256.Sum256([]byte(strings.ToLower(raw)))
			out.idHash = hex.EncodeToString(sum[:])[:16]
		}
	}
	for _, a := range req.Addresses {
		out.addrsN = append(out.addrsN, NormalizeAddress(a))
	}
	return out
}

func scoreOne(req normalizedReq, e domain.ListEntry) domain.HitInfo {
	matchedOn := []string{}
	conf := 0

	// 1) ID 哈希 — 单字段近乎确定
	if req.idHash != "" {
		for _, d := range e.IDDocuments {
			if d.NumberH == req.idHash &&
				(req.idType == "" || req.idType == strings.ToUpper(d.Type)) &&
				(req.idCountry == "" || req.idCountry == NormalizeCountry(d.Country)) {
				conf += 90
				matchedOn = append(matchedOn, "id_number")
				break
			}
		}
	}

	// 2) name primary
	primaryScore := JaroWinkler(req.nameN, NormalizeName(e.PrimaryName))
	if primaryScore >= 0.85 {
		conf += int(primaryScore * 60)
		matchedOn = append(matchedOn, "name")
	} else {
		// 3) 找 alias
		for _, alias := range e.Aliases {
			s := JaroWinkler(req.nameN, NormalizeName(alias))
			if s >= 0.85 {
				conf += int(s * 55)
				matchedOn = append(matchedOn, "alias")
				break
			}
		}
	}

	// 没名字命中且没 ID 命中 — 直接 drop, 不算 hit
	if conf == 0 {
		return domain.HitInfo{Confidence: 0}
	}

	// 4) DOB 完全匹配 (含部分匹配: 只给 YYYY 也认)
	if req.dob != "" && e.DOB != "" {
		switch {
		case req.dob == e.DOB:
			conf += 20
			matchedOn = append(matchedOn, "dob")
		case len(req.dob) >= 4 && len(e.DOB) >= 4 && req.dob[:4] == e.DOB[:4]:
			conf += 10
			matchedOn = append(matchedOn, "dob_year")
		}
	}

	// 5) 国籍
	if req.nationality != "" {
		for _, n := range e.Nationality {
			if NormalizeCountry(n) == req.nationality {
				conf += 10
				matchedOn = append(matchedOn, "nationality")
				break
			}
		}
	}

	// 6) 地址 (任一命中, 0.8 阈值)
	for _, ra := range req.addrsN {
		for _, ea := range e.Addresses {
			if JaroWinkler(ra, NormalizeAddress(ea)) >= 0.8 {
				conf += 10
				matchedOn = append(matchedOn, "address")
				goto addrDone
			}
		}
	}
addrDone:

	if conf > 100 {
		conf = 100
	}
	return domain.HitInfo{
		ListEntry:  e,
		Confidence: conf,
		MatchedOn:  matchedOn,
		Algorithm:  "jaro_winkler",
		State:      domain.HitPendingReview,
	}
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
