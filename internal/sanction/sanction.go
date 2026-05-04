// Package sanction AML / 制裁名单筛查。
//
// 商业化合规硬要求（进美欧市场不能跳过）：
//   - OFAC SDN (US Office of Foreign Assets Control - Specially Designated Nationals)
//   - EU Consolidated List
//   - UN Security Council Sanctions
//   - 国家自定义名单（中国 反洗钱中心 / UK HMT 等）
//
// 触发即必须 DENY，运营不能 override（合规要求）。
//
// 数据源：政府每天 / 每周更新公开 CSV/XML 文件，需要每日定时拉 → 解析 → 重建索引。
// 当前实现内存版，启动时从本地 file 加载；生产对接：
//
//	1. cron job 每 24h 从官方源拉最新 CSV/XML，落 S3
//	2. risk-manage 起一个 watcher：S3 文件变了就 reload
//	3. 命中后写 audit + 通知合规团队（webhook 给 compliance@）
//
// 匹配方式（fuzzy）：
//   - 标准化：去除标点 / 多余空格 / Unicode 折叠 / 转 lowercase
//   - 全名 exact match → 高置信
//   - Token 顺序无关（"John Doe" 命中 "Doe, John"）
//   - Levenshtein distance ≤ N → 中置信（拼写变体）
//   - 日期 + 国家 多字段匹配置信度更高
//
// 当前简化版只做"标准化全名 exact match + token bag match"；fuzzy 留 P1。
package sanction

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Source 名单来源。审计 + 报告用。
type Source string

const (
	SourceOFAC Source = "ofac_sdn"
	SourceEU   Source = "eu_consolidated"
	SourceUN   Source = "un_sc"
	SourceHMT  Source = "uk_hmt"  // 英国
	SourceCN   Source = "cn_pboc" // 中国 反洗钱中心
)

// Entry 一条制裁个人 / 实体。
type Entry struct {
	Source     Source    `json:"source"`
	UID        string    `json:"uid"`         // 来源系统的唯一 ID（OFAC SDN UID / EU 编号）
	Name       string    `json:"name"`        // 标准化前的原始全名
	Aliases    []string  `json:"aliases"`     // 别名 / 拼写变体
	Type       string    `json:"type"`        // individual | entity | vessel | aircraft
	Country    string    `json:"country"`     // ISO-2，可能多个
	BirthDate  string    `json:"birth_date"`  // YYYY-MM-DD（individual）
	Programs   []string  `json:"programs"`    // 触发的制裁项目代码（SDGT / IRAQ2 等）
	AddedAt    time.Time `json:"added_at"`    // 加到名单的时间
}

// Match 命中结果。多名单同时命中时所有 hits 都返回。
type Match struct {
	Hits       []*Entry
	NormalizedName string // 标准化后的查询名（debug 用）
}

// Service 名单查询接口。
type Service interface {
	// Check 查询 (full_name, country) 在所有名单里的命中。country 可空。
	// 实现保证 < 1ms（启动时建索引，O(1) hash + token bag scan）。
	Check(ctx context.Context, fullName, country string) *Match
	// Reload 重载所有 entries（cron 拉新数据后调）。原子替换。
	Reload(ctx context.Context, entries []*Entry) error
	// Stats 当前索引大小（admin 监控用）。
	Stats() (totalEntries int, lastReloadAt time.Time)
}

// MemService 内存版。生产部署：
//   - 启动从 S3 / file 加载完整名单
//   - watcher cron 每 1h 检查 file mtime；变了就 Reload
//   - Reload 是 O(N) 单次构建，N≈10万（OFAC 全量），< 100ms
type MemService struct {
	mu         sync.RWMutex
	exact      map[string][]*Entry // normalize(name) → entries（含 aliases）
	tokens     map[string][]*Entry // 单 token → 包含此 token 的 entries
	all        []*Entry
	lastReload time.Time
	totalScans uint64 // 调用 Check 的次数（内部 metrics）
}

func NewMemService() *MemService {
	return &MemService{
		exact:  make(map[string][]*Entry),
		tokens: make(map[string][]*Entry),
	}
}

// LoadCSVFile 从一个简化 CSV 文件加载 entries：
//
//	source,uid,name,aliases(|sep),type,country,birth_date,programs(|sep)
//
// 生产对接 OFAC SDN xml.gz / EU XML 时换成专门 parser。
func LoadCSVFile(path string) ([]*Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseCSV(f)
}

func parseCSV(r io.Reader) ([]*Entry, error) {
	c := csv.NewReader(r)
	c.FieldsPerRecord = -1 // 容忍 trailing 空字段
	rows, err := c.ReadAll()
	if err != nil {
		return nil, err
	}
	out := make([]*Entry, 0, len(rows))
	for _, row := range rows {
		if len(row) < 3 {
			continue
		}
		e := &Entry{
			Source: Source(strings.TrimSpace(row[0])),
			UID:    strings.TrimSpace(row[1]),
			Name:   strings.TrimSpace(row[2]),
		}
		if len(row) > 3 && row[3] != "" {
			e.Aliases = splitPipe(row[3])
		}
		if len(row) > 4 {
			e.Type = strings.TrimSpace(row[4])
		}
		if len(row) > 5 {
			e.Country = strings.TrimSpace(row[5])
		}
		if len(row) > 6 {
			e.BirthDate = strings.TrimSpace(row[6])
		}
		if len(row) > 7 && row[7] != "" {
			e.Programs = splitPipe(row[7])
		}
		if e.Name == "" {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func splitPipe(s string) []string {
	parts := strings.Split(s, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Reload 原子重建索引。
func (s *MemService) Reload(_ context.Context, entries []*Entry) error {
	if entries == nil {
		return errors.New("sanction: nil entries")
	}
	exact := make(map[string][]*Entry, len(entries))
	tokens := make(map[string][]*Entry, len(entries)*3)
	all := make([]*Entry, 0, len(entries))

	add := func(name string, e *Entry) {
		n := normalizeName(name)
		if n == "" {
			return
		}
		exact[n] = append(exact[n], e)
		for _, t := range tokenize(n) {
			tokens[t] = append(tokens[t], e)
		}
	}

	for _, e := range entries {
		all = append(all, e)
		add(e.Name, e)
		for _, a := range e.Aliases {
			add(a, e)
		}
	}

	s.mu.Lock()
	s.exact = exact
	s.tokens = tokens
	s.all = all
	s.lastReload = time.Now().UTC()
	s.mu.Unlock()
	return nil
}

// Check 查询 fullName 是否命中名单。country 可空（只过滤同国家以避免 cross-country
// false positive）。多策略：
//   1. 标准化全名 exact 命中 → 直接返回
//   2. token bag：fullName 全部 token 都被某 entry 命中 → 命中（顺序无关）
func (s *MemService) Check(_ context.Context, fullName, country string) *Match {
	atomic.AddUint64(&s.totalScans, 1)
	n := normalizeName(fullName)
	if n == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	hits := make([]*Entry, 0, 4)
	seen := make(map[*Entry]struct{})
	add := func(e *Entry) {
		if _, ok := seen[e]; ok {
			return
		}
		if country != "" && e.Country != "" &&
			!strings.EqualFold(country, e.Country) {
			return
		}
		seen[e] = struct{}{}
		hits = append(hits, e)
	}

	// 1) exact
	for _, e := range s.exact[n] {
		add(e)
	}
	if len(hits) > 0 {
		return &Match{Hits: hits, NormalizedName: n}
	}

	// 2) token bag (查询的所有 token 都被同一 entry 包含)
	qTokens := tokenize(n)
	if len(qTokens) == 0 {
		return nil
	}
	candidates := s.tokens[qTokens[0]]
	for _, e := range candidates {
		entryTokens := mergeNameTokens(e)
		ok := true
		for _, q := range qTokens {
			if !containsToken(entryTokens, q) {
				ok = false
				break
			}
		}
		if ok {
			add(e)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	return &Match{Hits: hits, NormalizedName: n}
}

func (s *MemService) Stats() (int, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.all), s.lastReload
}

// normalizeName 转 lowercase + 去重 whitespace + 折叠常见标点 / Unicode 全角。
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// 去标点（替换为空格）
	s = punctRepl.Replace(s)
	// 折叠多余 whitespace
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

var punctRepl = strings.NewReplacer(
	",", " ", ".", " ", ";", " ", ":", " ",
	"'", " ", "\"", " ", "(", " ", ")", " ",
	"[", " ", "]", " ", "/", " ", "\\", " ",
	"-", " ",
)

func tokenize(normalized string) []string {
	if normalized == "" {
		return nil
	}
	parts := strings.Fields(normalized)
	// 排序去重让 containsToken 用二分（n 一般 < 5，O(n) 也行）
	sort.Strings(parts)
	out := parts[:0]
	last := ""
	for _, p := range parts {
		if p != last && len(p) > 1 { // 单字母 token 噪声大
			out = append(out, p)
			last = p
		}
	}
	return out
}

func mergeNameTokens(e *Entry) []string {
	all := []string{e.Name}
	all = append(all, e.Aliases...)
	merged := make([]string, 0, 8)
	for _, n := range all {
		merged = append(merged, tokenize(normalizeName(n))...)
	}
	sort.Strings(merged)
	return merged
}

func containsToken(sorted []string, q string) bool {
	for _, s := range sorted {
		if s == q {
			return true
		}
	}
	return false
}
