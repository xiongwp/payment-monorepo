// query.go — 跨 Redis(热) + ClickHouse(冷) 的统一查询接口。
//
// admin web 的 /api/v1/diffs/search 应该透明：
//
//   - 查询时间窗在 (now - 7d) 内 → 直接 Redis ZRANGEBYSCORE，毫秒级
//   - 跨度跨 7d → 拆 Redis(7d) + ClickHouse(剩余) UNION
//   - 全在 7d 之外 → 直接 ClickHouse 一次 SELECT
//
// 调用方不关心数据来自哪儿，只看 []*Diff + 总数。
//
// 查询 dimension：
//   - time_range: from/to (UTC)
//   - state: open / acked / resolved / false_positive / expired / "" (any)
//   - script_id: 精确匹配 (空=any)
//   - type: 精确匹配 (空=any)
//   - limit: ≤500
//
// 实现注意：ClickHouse 的 ReplacingMergeTree 在 SELECT 时需 FINAL 保证去重（同一
// diff_id 多次归档拿最新）。FINAL 慢一些但 OK，行数小。

package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"reconcile-system/internal/diffstate"
	"reconcile-system/internal/metrics"
)

// SearchOpts 跨层查询条件。
type SearchOpts struct {
	From     time.Time
	To       time.Time
	State    string // 空=any
	ScriptID string // 空=any
	Type     string // 空=any
	Limit    int    // 默认 100，max 500
}

// SearchResult 返查询结果 + 来源标签。
type SearchResult struct {
	Diffs       []*diffstate.Diff `json:"diffs"`
	Total       int               `json:"total"`
	HotCount    int               `json:"hot_count"`  // 来自 Redis
	ColdCount   int               `json:"cold_count"` // 来自 ClickHouse
	QueriedHot  bool              `json:"queried_hot"`
	QueriedCold bool              `json:"queried_cold"`
}

// Search 透明查询。auto-route Redis/CH。
func (a *Archiver) Search(ctx context.Context, opts SearchOpts) (*SearchResult, error) {
	if opts.Limit <= 0 || opts.Limit > 500 {
		opts.Limit = 100
	}
	if opts.To.IsZero() {
		opts.To = time.Now()
	}
	if opts.From.IsZero() {
		opts.From = opts.To.Add(-30 * 24 * time.Hour)
	}
	res := &SearchResult{Diffs: []*diffstate.Diff{}}
	defer func() {
		tier := "hot"
		if res.QueriedHot && res.QueriedCold {
			tier = "mixed"
		} else if res.QueriedCold {
			tier = "cold"
		}
		metrics.ArchiveSearchTotal.WithLabelValues(tier).Inc()
	}()

	hotCutoff := time.Now().Add(-a.cfg.HotWindow)
	// 时间窗与 hot 区间 (hotCutoff, now] 是否有交集
	wantHot := opts.To.After(hotCutoff)
	// 时间窗与 cold 区间 (-inf, hotCutoff] 是否有交集
	wantCold := opts.From.Before(hotCutoff)

	if wantHot {
		hot, err := a.searchHot(ctx, opts, hotCutoff)
		if err == nil {
			res.Diffs = append(res.Diffs, hot...)
			res.HotCount = len(hot)
			res.QueriedHot = true
		}
	}
	if wantCold {
		cold, err := a.searchCold(ctx, opts, hotCutoff)
		if err != nil {
			// CH 挂了不阻塞 hot 结果（部分降级）
			if !res.QueriedHot {
				return nil, fmt.Errorf("clickhouse search failed: %w", err)
			}
		} else {
			res.Diffs = append(res.Diffs, cold...)
			res.ColdCount = len(cold)
			res.QueriedCold = true
		}
	}
	res.Total = len(res.Diffs)
	// 简单按 updated_at 倒序
	sortByUpdatedDesc(res.Diffs)
	if res.Total > opts.Limit {
		res.Diffs = res.Diffs[:opts.Limit]
		res.Total = opts.Limit
	}
	return res, nil
}

// searchHot 走 Redis：ZRANGEBYSCORE 时间窗 + Get 详情 + filter 字段。
func (a *Archiver) searchHot(ctx context.Context, opts SearchOpts, hotCutoff time.Time) ([]*diffstate.Diff, error) {
	from := maxTime(opts.From, hotCutoff).UnixMilli()
	to := opts.To.UnixMilli()
	states := allStatesIfEmpty(opts.State)
	out := make([]*diffstate.Diff, 0, opts.Limit)
	for _, st := range states {
		ids, err := a.rdb.ZRangeByScore(ctx, "recon:diff:by_state:"+st,
			&redis.ZRangeBy{
				Min:   strconv.FormatInt(from, 10),
				Max:   strconv.FormatInt(to, 10),
				Count: int64(opts.Limit * 3), // 富余些防 filter 后不够
			}).Result()
		if err != nil {
			continue
		}
		for _, id := range ids {
			d, err := a.store.Get(ctx, id)
			if err != nil || d == nil {
				continue
			}
			if !matchFilter(d, opts) {
				continue
			}
			out = append(out, d)
			if len(out) >= opts.Limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// searchCold 走 ClickHouse：单条 SELECT FINAL + 时间窗 + WHERE + LIMIT。
func (a *Archiver) searchCold(ctx context.Context, opts SearchOpts, hotCutoff time.Time) ([]*diffstate.Diff, error) {
	to := minTime(opts.To, hotCutoff)
	if !opts.From.Before(to) {
		return nil, nil
	}
	var where []string
	args := map[string]string{}
	where = append(where, fmt.Sprintf("created_at >= toDateTime64('%s', 3)",
		opts.From.UTC().Format("2006-01-02 15:04:05.000")))
	where = append(where, fmt.Sprintf("created_at < toDateTime64('%s', 3)",
		to.UTC().Format("2006-01-02 15:04:05.000")))
	if opts.State != "" {
		where = append(where, "state = {state:String}")
		args["state"] = opts.State
	}
	if opts.ScriptID != "" {
		where = append(where, "script_id = {script_id:String}")
		args["script_id"] = opts.ScriptID
	}
	if opts.Type != "" {
		where = append(where, "type = {type:String}")
		args["type"] = opts.Type
	}
	q := fmt.Sprintf(`SELECT id, script_id, run_id, type, key, detail_json,
       state, toString(created_at), toString(updated_at),
       updated_by, note
FROM %s.%s FINAL
WHERE %s
ORDER BY updated_at DESC
LIMIT %d
FORMAT JSONEachRow`, a.cfg.Database, a.cfg.Table,
		strings.Join(where, " AND "), opts.Limit)

	body, err := a.execSelect(ctx, q, args)
	if err != nil {
		return nil, err
	}
	out := make([]*diffstate.Diff, 0, opts.Limit)
	dec := json.NewDecoder(bytes.NewReader(body))
	for dec.More() {
		var row struct {
			ID         string `json:"id"`
			ScriptID   string `json:"script_id"`
			RunID      string `json:"run_id"`
			Type       string `json:"type"`
			Key        string `json:"key"`
			DetailJSON string `json:"detail_json"`
			State      string `json:"state"`
			CreatedAt  string `json:"toString(created_at)"`
			UpdatedAt  string `json:"toString(updated_at)"`
			UpdatedBy  string `json:"updated_by"`
			Note       string `json:"note"`
		}
		if err := dec.Decode(&row); err != nil {
			break
		}
		d := &diffstate.Diff{
			ID:        row.ID,
			ScriptID:  row.ScriptID,
			RunID:     row.RunID,
			Type:      row.Type,
			Key:       row.Key,
			State:     diffstate.State(row.State),
			UpdatedBy: row.UpdatedBy,
			Note:      row.Note,
		}
		_ = json.Unmarshal([]byte(row.DetailJSON), &d.Detail)
		d.CreatedAt, _ = time.Parse("2006-01-02 15:04:05.000", row.CreatedAt)
		d.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05.000", row.UpdatedAt)
		out = append(out, d)
	}
	return out, nil
}

// execSelect POST query + named params, return raw response body.
func (a *Archiver) execSelect(ctx context.Context, query string, params map[string]string) ([]byte, error) {
	u, _ := url.Parse(a.cfg.URL)
	q := u.Query()
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	q.Set("default_format", "JSONEachRow")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	if a.cfg.User != "" {
		req.SetBasicAuth(a.cfg.User, a.cfg.Password)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("clickhouse SELECT %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// AggByDay 跨年月度聚合（"按天 diff 数量"），冷热分层用 SUM-of-parts 跨界拼接。
//
// admin web 用：画一年趋势图（hot=最近 7d 走 ZSET CARD by-day, cold 走 CH GROUP BY toDate）。
// 这里只做 cold 部分，hot 因为 7d 范围短，admin 直接用 stats.go 的 zcount。
func (a *Archiver) AggByDay(ctx context.Context, from, to time.Time, scriptID string) (map[string]int, error) {
	hotCutoff := time.Now().Add(-a.cfg.HotWindow)
	if to.After(hotCutoff) {
		to = hotCutoff
	}
	if !from.Before(to) {
		return map[string]int{}, nil
	}
	where := []string{
		fmt.Sprintf("created_at >= toDateTime64('%s', 3)",
			from.UTC().Format("2006-01-02 15:04:05.000")),
		fmt.Sprintf("created_at < toDateTime64('%s', 3)",
			to.UTC().Format("2006-01-02 15:04:05.000")),
	}
	args := map[string]string{}
	if scriptID != "" {
		where = append(where, "script_id = {script_id:String}")
		args["script_id"] = scriptID
	}
	q := fmt.Sprintf(`SELECT toString(toDate(created_at)) AS day, count() AS c
FROM %s.%s FINAL
WHERE %s
GROUP BY day
ORDER BY day
FORMAT JSONEachRow`, a.cfg.Database, a.cfg.Table, strings.Join(where, " AND "))
	body, err := a.execSelect(ctx, q, args)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	dec := json.NewDecoder(bytes.NewReader(body))
	for dec.More() {
		var row struct {
			Day string `json:"day"`
			C   any    `json:"c"`
		}
		if err := dec.Decode(&row); err != nil {
			break
		}
		// CH JSON int 是 string，避大数精度丢
		switch v := row.C.(type) {
		case string:
			n, _ := strconv.Atoi(v)
			out[row.Day] = n
		case float64:
			out[row.Day] = int(v)
		}
	}
	return out, nil
}

// ─── helpers ─────────────────────────────────────────────────────────

func matchFilter(d *diffstate.Diff, opts SearchOpts) bool {
	if opts.State != "" && string(d.State) != opts.State {
		return false
	}
	if opts.ScriptID != "" && d.ScriptID != opts.ScriptID {
		return false
	}
	if opts.Type != "" && d.Type != opts.Type {
		return false
	}
	return true
}

func allStatesIfEmpty(s string) []string {
	if s != "" {
		return []string{s}
	}
	return []string{"open", "acked", "resolved", "false_positive", "expired"}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func sortByUpdatedDesc(ds []*diffstate.Diff) {
	// 简单冒泡 — 100 条规模够
	for i := 0; i < len(ds); i++ {
		for j := i + 1; j < len(ds); j++ {
			if ds[j].UpdatedAt.After(ds[i].UpdatedAt) {
				ds[i], ds[j] = ds[j], ds[i]
			}
		}
	}
}
