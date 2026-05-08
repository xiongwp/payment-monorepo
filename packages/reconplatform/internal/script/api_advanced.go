// 复杂场景 API 扩展：让脚本能写复杂对账逻辑而不是堆 for 循环。
//
// 这些 helper 全部建立在 search 之上，但提供更"业务化"的语义：
//
//   1. JoinByIndex：多 idx 维度 JOIN（"按 pi_id 关联，再按 merchant_id 二次过滤"）
//   2. GroupBy：分组聚合（"按商户分组求金额合计"）
//   3. SumBy / CountBy：常见聚合（"过去 1h 总金额 / 总笔数"）
//   4. TimeWindow：时间窗口过滤（"只看 created_at 在 [since, until] 内的"）
//   5. SQL：直接对业务库跑 SELECT（少用，但有时需要拿 Redis 没有的 detail，
//          如 audit_log 全文）
//   6. HTTPGet：调外部接口（如查 channel 真实状态做 cross-check）
//   7. CompareCols：两个 event 多列对比，自动生成 diff
//
// 所有 API 都会被 yaegi 注入到 "recon" import 包里，脚本可以直接用：
//
//	import "recon"
//	groups := ctx.GroupBy(events, "merchant_id")
//	for mid, evs := range groups {
//	    total := evs.SumInt("amount")
//	    if total > merchantLimit(mid) { ctx.AddDiff(...) }
//	}
package script

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"reconcile-system/internal/store"
)

// ─── 1. Join：多索引交叉查询 ─────────────────────────────────────

// JoinByIndex 交集查询：同时匹配多个索引的事件。
//
// 例：要找"商户 m_xxx 在 2026-05-08 这天的所有 PI"：
//   events := ctx.JoinByIndex(map[string]string{
//       "merchant_id": "m_xxx",
//       "created_date": "2026-05-08",
//   })
// （前提：cdc.Source.IndexColumns 配了 ["merchant_id", "created_date"]）
//
// 实现：拿每个 idx 的 SET → 求交集 → MGET 主存。
func (c *Context) JoinByIndex(filters map[string]string) store.EventList {
	if len(filters) == 0 {
		return nil
	}
	c.stats.IndexLookups += len(filters)

	// 收集每个 filter 命中的 SET（"<svc>:<table>:<pk>" 列表）
	sets := make([][]string, 0, len(filters))
	for idx, val := range filters {
		members, err := c.searcher.SMembersByIndex(c.Ctx, idx, val)
		if err != nil {
			c.logger.Warn("JoinByIndex SET failed", "idx", idx, "value", val, "err", err.Error())
			c.stats.RedisErrors++
			return nil
		}
		if len(members) == 0 {
			return nil // 任意一个 filter 空 → 交集必然空
		}
		sets = append(sets, members)
	}

	// 多路求交集（按最小集合驱动）
	sort.Slice(sets, func(i, j int) bool { return len(sets[i]) < len(sets[j]) })
	intersection := make(map[string]struct{}, len(sets[0]))
	for _, m := range sets[0] {
		intersection[m] = struct{}{}
	}
	for i := 1; i < len(sets); i++ {
		next := make(map[string]struct{}, len(intersection))
		for _, m := range sets[i] {
			if _, ok := intersection[m]; ok {
				next[m] = struct{}{}
			}
		}
		intersection = next
		if len(intersection) == 0 {
			return nil
		}
	}

	// 批量取主存
	refs := make([]string, 0, len(intersection))
	for r := range intersection {
		refs = append(refs, r)
	}
	events, err := c.searcher.GetEventsByRefs(c.Ctx, refs)
	if err != nil {
		c.logger.Warn("JoinByIndex MGET failed", "err", err.Error())
		c.stats.RedisErrors++
	}
	return events
}

// ─── 2. GroupBy ─────────────────────────────────────────────────

// GroupBy 按 events 行内某列分组。col 不存在时分到 "" 桶。
func (c *Context) GroupBy(events store.EventList, col string) map[string]store.EventList {
	out := make(map[string]store.EventList)
	for _, e := range events {
		key := e.Str(col)
		out[key] = append(out[key], e)
	}
	return out
}

// GroupByPair 按两列联合分组（例：merchant_id + currency）。
func (c *Context) GroupByPair(events store.EventList, col1, col2 string) map[[2]string]store.EventList {
	out := make(map[[2]string]store.EventList)
	for _, e := range events {
		key := [2]string{e.Str(col1), e.Str(col2)}
		out[key] = append(out[key], e)
	}
	return out
}

// ─── 3. 聚合函数 ────────────────────────────────────────────────

// SumInt 聚合 events[*].Row[col] 为 int64 求和。
func (c *Context) SumInt(events store.EventList, col string) int64 {
	var sum int64
	for _, e := range events {
		sum += e.Int(col)
	}
	return sum
}

// CountWhere 满足谓词的 events 数量。
//
// 例：count := ctx.CountWhere(events, func(e *store.Event) bool {
//     return e.Str("status") == "failed"
// })
func (c *Context) CountWhere(events store.EventList, pred func(*store.Event) bool) int {
	cnt := 0
	for _, e := range events {
		if pred(e) {
			cnt++
		}
	}
	return cnt
}

// Filter 谓词过滤。
func (c *Context) Filter(events store.EventList, pred func(*store.Event) bool) store.EventList {
	out := make(store.EventList, 0, len(events))
	for _, e := range events {
		if pred(e) {
			out = append(out, e)
		}
	}
	return out
}

// ─── 4. 时间窗口 ────────────────────────────────────────────────

// InTimeWindow 过滤 events[*].Timestamp 在 [since, until] 之间。
// CDC publisher 写入的 e.Timestamp 是 binlog event 时间，单调递增。
func (c *Context) InTimeWindow(events store.EventList, since, until time.Time) store.EventList {
	out := make(store.EventList, 0, len(events))
	for _, e := range events {
		if (e.Timestamp.Equal(since) || e.Timestamp.After(since)) &&
			(e.Timestamp.Equal(until) || e.Timestamp.Before(until)) {
			out = append(out, e)
		}
	}
	return out
}

// ─── 5. SQL 直查 ────────────────────────────────────────────────

// SQLDB 给脚本暴露的 SELECT-only 数据库句柄。Loader 在 ctx 注入时用
// 业务库的只读账号；脚本不能 write（DML 在 driver 侧拒绝？这里只是约定，
// 真生产要拿一个权限收紧的账号）。
type SQLDB struct {
	db *sql.DB
}

// QueryRows 执行 SELECT，返回 []map[col]val。脚本仅 SELECT 用。
//
// 用例：拿 audit_log 全文（CDC 默认不同步太大的列）做合规审计：
//   rows, _ := ctx.SQL("order-core", "shard5").
//       Query("SELECT id, payload FROM audit_log WHERE ts > ? LIMIT 100", since)
func (s *SQLDB) Query(q string, args ...any) ([]map[string]any, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("SQLDB not configured for this script")
	}
	q = strings.TrimSpace(q)
	if !strings.HasPrefix(strings.ToUpper(q), "SELECT") {
		return nil, fmt.Errorf("only SELECT allowed, got: %.20q", q)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, 64)
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return out, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			v := raw[i]
			if b, ok := v.([]byte); ok {
				v = string(b) // VARCHAR / TEXT 来的都是 []byte
			}
			m[c] = v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SQL 拿一个 service+shard 的 SQLDB（脚本里直接 ctx.SQL("order-core", "shard0")）。
// 实际 db 句柄由 Context 注入时持有。
func (c *Context) SQL(service, shard string) *SQLDB {
	c.stats.SQLQueries++
	if c.sqlDBs == nil {
		return nil
	}
	key := service + "/" + shard
	return c.sqlDBs[key]
}

// ─── 6. HTTP 外调 ───────────────────────────────────────────────

// HTTPGet 调外部 API（限 GET，超时 5s）。脚本里偶尔需要查渠道实际状态、
// 三方反欺诈分数等 Redis 没有的数据。
//
// 失败 / 超时 → 返 nil + log warn，不 panic。
func (c *Context) HTTPGet(url string) []byte {
	c.stats.HTTPCalls++
	cli := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(c.Ctx, "GET", url, nil)
	if err != nil {
		c.logger.Warn("HTTPGet build request failed", "url", url, "err", err.Error())
		return nil
	}
	resp, err := cli.Do(req)
	if err != nil {
		c.logger.Warn("HTTPGet failed", "url", url, "err", err.Error())
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.logger.Warn("HTTPGet read body", "url", url, "err", err.Error())
		return nil
	}
	if resp.StatusCode >= 400 {
		c.logger.Warn("HTTPGet non-2xx", "url", url, "status", resp.StatusCode)
	}
	return body
}

// HTTPGetJSON 同 HTTPGet，但解析成 map 给脚本直接用。
func (c *Context) HTTPGetJSON(url string) map[string]any {
	body := c.HTTPGet(url)
	if len(body) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		c.logger.Warn("HTTPGetJSON unmarshal", "url", url, "err", err.Error())
		return nil
	}
	return m
}

// ─── 7. 列对比 helper ───────────────────────────────────────────

// CompareCols 两个 event 在指定列集合上对比；任一列不一致就 AddCompare 一条 diff。
//
// 例：ctx.CompareCols("amount_or_status", piID,
//        events.Find("order-core","payment_intents"),
//        events.Find("payment-channel","card_charges"),
//        []string{"amount", "currency", "status"})
func (c *Context) CompareCols(diffType, key string, a, b *store.Event, cols []string) bool {
	if a.IsEmpty() || b.IsEmpty() {
		return false
	}
	mismatches := make(map[string]any)
	for _, col := range cols {
		va := a.Row()[col]
		vb := b.Row()[col]
		if !equalAny(va, vb) {
			mismatches[col] = map[string]any{"a": va, "b": vb}
		}
	}
	if len(mismatches) > 0 {
		c.AddDiff(diffType, key, mismatches)
		return true
	}
	return false
}

// equalAny 简单等值判断；string / 数字 / nil 都覆盖。
// MySQL 不同列类型来到 Go 后可能是 string(20) vs int64(20)，这里做了类型容忍。
func equalAny(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a == b {
		return true
	}
	// 数值 / 字符串容忍
	sa := fmt.Sprintf("%v", a)
	sb := fmt.Sprintf("%v", b)
	return sa == sb
}

// ─── 8. Diff helper：批量提交 ─────────────────────────────────

// CommitDiffs 一次性提交 diff 列表（脚本里循环外面用）。
func (c *Context) CommitDiffs(diffs []Diff) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diffs = append(c.diffs, diffs...)
}

// ─── ContextOptions：注入更复杂的依赖 ───────────────────────

// ContextOptions Loader 在构造 Context 时可以传：业务库句柄 / 自定义 logger / 等。
//
// SQLDBs key 形态："<service>/<shard>" → *sql.DB
// 实际生产里 reconplatform 启动时为每个 service 的每个 shard 都开 1 个低池
// 化 sql.DB（max_conn=2 即可，对账查询 RPS 远低于业务）。
type ContextOptions struct {
	SQLDBs map[string]*sql.DB
}

// 让 context 包正确导入（被 store 间接用）
var _ = context.Background
