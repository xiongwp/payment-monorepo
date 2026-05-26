// rule_versions.go: 规则版本化（git-like history）。
//
// 每条 rule 改动都写一个 immutable version 到 RuleVersionStore；rollback =
// 切 active pointer（不动 history）。Engine.UpdateRule 在内存替换前先调
// Record + Activate，确保审计 / 回滚总能反查到 spec 原文。
//
// 跟 RuleAuditEntry 互补，不替代：
//   - RuleAuditEntry 记"动作+actor+before/after diff"
//   - RuleVersion    记"每个 (rule_id, version) 的完整 spec_json 不可变快照"
//
// 默认实现 MemRuleVersionStore（进程内 map；进程重启清空）。生产换 PG
// 实现（见 internal/store/postgres_rule_versions.go，带 //go:build pg）。
//
// content_hash 用 canonical JSON（key 排序后）的 sha256，所以"键序不同
// 但内容相同"的 spec 不会重复写 version。

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RuleVersion 一条规则的某个不可变版本快照。
type RuleVersion struct {
	RuleID        string    `json:"rule_id"`
	Version       int64     `json:"version"`
	ContentHash   string    `json:"content_hash"` // sha256(canonical(spec_json))
	SpecJSON      []byte    `json:"spec_json"`
	ChangeSummary string    `json:"change_summary,omitempty"`
	Author        string    `json:"author,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// RuleVersionStore 规则版本持久化接口。Mem / PG 可换。
type RuleVersionStore interface {
	// Record 写新版本。内容 hash 跟同 rule_id 的最新版本一致时直接复用旧
	// version（不写新行），isNew=false；否则分配新 version、isNew=true。
	Record(ctx context.Context, ruleID string, spec []byte, summary, author string) (version int64, isNew bool, err error)
	// Activate 把 (ruleID, version) 设为当前 active。rollback 也走这里
	// （只切指针，不动 history）。
	Activate(ctx context.Context, ruleID string, version int64, actor string) error
	// GetActive 当前 active 版本（version=0, spec=nil 表示 rule_id 无版本）。
	GetActive(ctx context.Context, ruleID string) (version int64, spec []byte, err error)
	// ListVersions 历史版本（按 version 降序，limit<=0 → 100）。
	ListVersions(ctx context.Context, ruleID string, limit int) ([]RuleVersion, error)
	// GetVersion 取某个具体 (rule_id, version) 的 spec 快照。
	GetVersion(ctx context.Context, ruleID string, version int64) (RuleVersion, error)
	// Diff 两版本之间 spec_json 的行级 unified diff。
	Diff(ctx context.Context, ruleID string, fromVer, toVer int64) (string, error)
}

// ErrVersionNotFound 没找到指定版本（rollback / diff / list 用）。
var ErrVersionNotFound = errors.New("rule version not found")

// CanonicalJSON 把 raw 重新 marshal 成"key 递归排序"的 JSON。空输入 → 空。
// 用途：内容 hash 稳定不依赖键序。
func CanonicalJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		// 不是合法 JSON 时退化成原文（rule 的 ConfigJSON 偶尔可能为 nil 或空 string）。
		return raw, nil
	}
	return canonicalMarshal(v)
}

func canonicalMarshal(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			vb, err := canonicalMarshal(t[k])
			if err != nil {
				return nil, err
			}
			b.Write(vb)
		}
		b.WriteByte('}')
		return []byte(b.String()), nil
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, it := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			vb, err := canonicalMarshal(it)
			if err != nil {
				return nil, err
			}
			b.Write(vb)
		}
		b.WriteByte(']')
		return []byte(b.String()), nil
	default:
		return json.Marshal(v)
	}
}

// ContentHash spec_json 的稳定 hex sha256（先做 CanonicalJSON）。
func ContentHash(spec []byte) string {
	c, _ := CanonicalJSON(spec)
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:])
}

// MemRuleVersionStore 进程内版本存储。重启清空；prod 用 PG 替代。
type MemRuleVersionStore struct {
	mu       sync.RWMutex
	versions map[string][]RuleVersion // ruleID → 按 version 升序
	active   map[string]int64         // ruleID → active version
	nextVer  map[string]int64         // ruleID → next-to-assign version
}

// NewMemRuleVersionStore 空 store。
func NewMemRuleVersionStore() *MemRuleVersionStore {
	return &MemRuleVersionStore{
		versions: make(map[string][]RuleVersion),
		active:   make(map[string]int64),
		nextVer:  make(map[string]int64),
	}
}

func (s *MemRuleVersionStore) Record(_ context.Context, ruleID string, spec []byte, summary, author string) (int64, bool, error) {
	if ruleID == "" {
		return 0, false, errors.New("rule_id required")
	}
	hash := ContentHash(spec)
	s.mu.Lock()
	defer s.mu.Unlock()
	// 跟同 rule_id 的最新版本（任意已存在版本中的 hash）一致 → 复用
	if vs := s.versions[ruleID]; len(vs) > 0 {
		// 看最新一条（list 升序，末尾即最新）。
		last := vs[len(vs)-1]
		if last.ContentHash == hash {
			return last.Version, false, nil
		}
	}
	next := s.nextVer[ruleID] + 1
	s.nextVer[ruleID] = next
	// 拷贝 spec 避免外部 mutate。
	specCp := make([]byte, len(spec))
	copy(specCp, spec)
	rv := RuleVersion{
		RuleID:        ruleID,
		Version:       next,
		ContentHash:   hash,
		SpecJSON:      specCp,
		ChangeSummary: summary,
		Author:        author,
		CreatedAt:     time.Now().UTC(),
	}
	s.versions[ruleID] = append(s.versions[ruleID], rv)
	return next, true, nil
}

func (s *MemRuleVersionStore) Activate(_ context.Context, ruleID string, version int64, _ string) error {
	if ruleID == "" {
		return errors.New("rule_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, rv := range s.versions[ruleID] {
		if rv.Version == version {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: rule_id=%s version=%d", ErrVersionNotFound, ruleID, version)
	}
	s.active[ruleID] = version
	return nil
}

func (s *MemRuleVersionStore) GetActive(_ context.Context, ruleID string) (int64, []byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ver, ok := s.active[ruleID]
	if !ok {
		return 0, nil, nil
	}
	for _, rv := range s.versions[ruleID] {
		if rv.Version == ver {
			cp := make([]byte, len(rv.SpecJSON))
			copy(cp, rv.SpecJSON)
			return ver, cp, nil
		}
	}
	return 0, nil, ErrVersionNotFound
}

func (s *MemRuleVersionStore) ListVersions(_ context.Context, ruleID string, limit int) ([]RuleVersion, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	vs := s.versions[ruleID]
	n := len(vs)
	if n == 0 {
		return nil, nil
	}
	if limit > n {
		limit = n
	}
	out := make([]RuleVersion, 0, limit)
	// version 降序：尾→头
	for i := 0; i < limit; i++ {
		rv := vs[n-1-i]
		// 拷贝 spec 防 mutate
		cp := make([]byte, len(rv.SpecJSON))
		copy(cp, rv.SpecJSON)
		rv.SpecJSON = cp
		out = append(out, rv)
	}
	return out, nil
}

func (s *MemRuleVersionStore) GetVersion(_ context.Context, ruleID string, version int64) (RuleVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rv := range s.versions[ruleID] {
		if rv.Version == version {
			cp := make([]byte, len(rv.SpecJSON))
			copy(cp, rv.SpecJSON)
			rv.SpecJSON = cp
			return rv, nil
		}
	}
	return RuleVersion{}, fmt.Errorf("%w: rule_id=%s version=%d", ErrVersionNotFound, ruleID, version)
}

func (s *MemRuleVersionStore) Diff(ctx context.Context, ruleID string, fromVer, toVer int64) (string, error) {
	a, err := s.GetVersion(ctx, ruleID, fromVer)
	if err != nil {
		return "", err
	}
	b, err := s.GetVersion(ctx, ruleID, toVer)
	if err != nil {
		return "", err
	}
	return UnifiedDiff(prettyJSON(a.SpecJSON), prettyJSON(b.SpecJSON),
		fmt.Sprintf("%s@%d", ruleID, fromVer),
		fmt.Sprintf("%s@%d", ruleID, toVer)), nil
}

// prettyJSON 把 raw 按 2-space indent 重排，方便 diff 出"字段级"差异而非
// 整行。raw 非合法 JSON 时退化原样返回。
func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	c, err := CanonicalJSON(raw)
	if err != nil || len(c) == 0 {
		return string(raw)
	}
	var v any
	if err := json.Unmarshal(c, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// UnifiedDiff 行级 unified diff（最朴素的 LCS-based 实现；不依赖外部库）。
// header 形如 "--- aName\n+++ bName\n"；行前缀 " " / "-" / "+"。
//
// 不输出 hunk 偏移（@@ -1,3 +1,3 @@），简化实现；调试 / API 够用。
// 真正给运营看的 diff 端到端用 prettyJSON → 大致是字段级。
func UnifiedDiff(a, b, aName, bName string) string {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	// 经典 LCS 表
	n, m := len(al), len(bl)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var b2 strings.Builder
	b2.WriteString("--- ")
	b2.WriteString(aName)
	b2.WriteByte('\n')
	b2.WriteString("+++ ")
	b2.WriteString(bName)
	b2.WriteByte('\n')
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			b2.WriteString(" ")
			b2.WriteString(al[i])
			b2.WriteByte('\n')
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			b2.WriteString("-")
			b2.WriteString(al[i])
			b2.WriteByte('\n')
			i++
		default:
			b2.WriteString("+")
			b2.WriteString(bl[j])
			b2.WriteByte('\n')
			j++
		}
	}
	for ; i < n; i++ {
		b2.WriteString("-")
		b2.WriteString(al[i])
		b2.WriteByte('\n')
	}
	for ; j < m; j++ {
		b2.WriteString("+")
		b2.WriteString(bl[j])
		b2.WriteByte('\n')
	}
	return b2.String()
}

// SetRuleVersionStore 注入版本 store。nil 表示不做版本化（engine 退化到
// 老路径，UpdateRule 直接替换内存）。
func (e *Engine) SetRuleVersionStore(s RuleVersionStore) {
	e.mu.Lock()
	e.versionStore = s
	e.mu.Unlock()
}

// VersionStore 当前注入的版本 store（admin endpoint 用）。
func (e *Engine) VersionStore() RuleVersionStore {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.versionStore
}

// RuleSetHash 当前 engine 规则集的稳定 hash（32-bit），用于
// audit.DecisionAudit.RuleVersion。每条 rule 的 (id, active_version) 都
// 进 hash；版本 store 缺失时退化到 (id, 0)。空规则集 → 0。
//
// 32-bit 而非 64-bit 是为兼容 audit.RuleVersion int 字段（clickhouse_sink
// 强转 int32）。
func (e *Engine) RuleSetHash() uint32 {
	e.mu.RLock()
	store := e.versionStore
	ids := make([]string, 0, len(e.defs))
	for _, d := range e.defs {
		ids = append(ids, d.ID)
	}
	e.mu.RUnlock()
	if len(ids) == 0 {
		return 0
	}
	sort.Strings(ids)
	h := fnv.New32a()
	for _, id := range ids {
		var ver int64
		if store != nil {
			ver, _, _ = store.GetActive(context.Background(), id)
		}
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strconv.FormatInt(ver, 10)))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum32()
}

// RuleSetHashHex 64-bit-friendly hex 表示（给 admin / debug 用）。
func (e *Engine) RuleSetHashHex() string {
	return strconv.FormatUint(uint64(e.RuleSetHash()), 16)
}

// UpdateRuleVersioned 单条规则原子热更新 + 版本化。先 Record + Activate，
// 成功后才替换内存；任一步失败 → 内存保持不变，返 error。
//
// 没注入 versionStore 时退化到 UpdateRule 的老路径（无版本记录）。
//
// 返回：
//
//	updated   true=更新；false=新增
//	version   分配 / 复用 的版本号（0 = versionStore=nil）
//	isNew     是否真的写了新版本（false = content_hash 跟上一次一致）
func (e *Engine) UpdateRuleVersioned(ctx context.Context, d RuleDef, r Rule, summary, author string) (updated bool, version int64, isNew bool, err error) {
	e.mu.RLock()
	store := e.versionStore
	e.mu.RUnlock()
	if store != nil {
		// spec_json: 整条 RuleDef 序列化（含 mode / weight / enabled / config）。
		// 后续 rollback 也是反序列化 RuleDef 重 build。
		spec, mErr := json.Marshal(d)
		if mErr != nil {
			return false, 0, false, fmt.Errorf("marshal spec: %w", mErr)
		}
		ver, isNewV, rErr := store.Record(ctx, d.ID, spec, summary, author)
		if rErr != nil {
			return false, 0, false, fmt.Errorf("version record: %w", rErr)
		}
		if aErr := store.Activate(ctx, d.ID, ver, author); aErr != nil {
			return false, 0, false, fmt.Errorf("version activate: %w", aErr)
		}
		version = ver
		isNew = isNewV
	}
	// 老路径：内存替换。返回 updated bool 跟之前一致。
	updated = e.UpdateRule(d, r)
	return updated, version, isNew, nil
}
