// Package store 已有 Redis 客户端封装。本文件加跨服务搜索能力，给：
//
//	1. recon.Context.GetByIndex（脚本运行时用）
//	2. /api/v1/search?index=order_id&value=xxx（编辑器实时搜索）
//
// 核心入口：SearchByIndex(idx, val) → []Event。
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Event 跟 cdc.Event 形态一致；这里独立定义避免 store → cdc 反向依赖
// （store 是底层；脚本和 api 通过 store 拿数据）。
type Event struct {
	Service    string            `json:"svc"`
	Schema     string            `json:"db"`
	Table      string            `json:"table"`
	PK         string            `json:"pk"`
	Op         string            `json:"op"`
	Before     map[string]any    `json:"before,omitempty"`
	After      map[string]any    `json:"after,omitempty"`
	BinlogFile string            `json:"binlog_file"`
	BinlogPos  uint32            `json:"binlog_pos"`
	GTID       string            `json:"gtid,omitempty"`
	Timestamp  time.Time         `json:"ts"`
	Indexes    map[string]string `json:"indexes,omitempty"`
}

// Row 取行数据（UPDATE/INSERT 用 After，DELETE 用 Before）。
func (e *Event) Row() map[string]any {
	if len(e.After) > 0 {
		return e.After
	}
	return e.Before
}

// IsEmpty 没拿到事件（比如某腿缺失）。
func (e *Event) IsEmpty() bool {
	return e == nil || (e.Service == "" && e.PK == "")
}

// Int 取行内某列的 int64 值（任意数值类型转 int64，nil → 0）。
func (e *Event) Int(col string) int64 {
	if e == nil {
		return 0
	}
	row := e.Row()
	v, ok := row[col]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		// 容忍 string 形态的整数（CDC parser normalize 可能把数字转 string）
		var n int64
		_, _ = fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

// Str 取行内某列的字符串值；nil → ""。
func (e *Event) Str(col string) string {
	if e == nil {
		return ""
	}
	row := e.Row()
	v, ok := row[col]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// EventList 一组事件，提供链式查询 helper（脚本写得舒服一点）。
type EventList []*Event

// Find 找第一条 (service, table) 匹配的事件；找不到返 IsEmpty() 的 Event。
func (es EventList) Find(service, table string) *Event {
	for _, e := range es {
		if e.Service == service && e.Table == table {
			return e
		}
	}
	return &Event{}
}

// FindAll 找所有 (service, table) 匹配的（一对多场景：一个 pi_id 对应多条 charge）。
func (es EventList) FindAll(service, table string) EventList {
	var out EventList
	for _, e := range es {
		if e.Service == service && e.Table == table {
			out = append(out, e)
		}
	}
	return out
}

// ByService 按服务名分组。
func (es EventList) ByService() map[string]EventList {
	out := make(map[string]EventList)
	for _, e := range es {
		out[e.Service] = append(out[e.Service], e)
	}
	return out
}

// Searcher 跨服务索引搜索 + 主存读取。
type Searcher struct {
	r redis.UniversalClient
}

// NewSearcher 构造（caller 传入已 dial 的 redis client）。
func NewSearcher(r redis.UniversalClient) *Searcher {
	return &Searcher{r: r}
}

// SearchByIndex 用业务 key (idx_name, value) 拿到所有引用它的事件。
//
//	idx_name 例：order_id / pi_id / transaction_id / merchant_id
//	value    例：pi_xxx / ord_yyy / m_zzz
//
// 步骤：
//   1. SMEMBERS recon:idx:<idx>:<val>  → ["order-core:payment_intents:pi_xxx", ...]
//   2. MGET recon:evt:<svc>:<table>:<pk>  ×N （pipeline 1 RTT）
//   3. JSON 解析
//
// 中途 nil（事件已过期）跳过，不报错。
func (s *Searcher) SearchByIndex(ctx context.Context, idxName, value string) (EventList, error) {
	idxKey := fmt.Sprintf("recon:idx:%s:%s", idxName, value)
	members, err := s.r.SMembers(ctx, idxKey).Result()
	if err != nil {
		return nil, fmt.Errorf("smembers %s: %w", idxKey, err)
	}
	if len(members) == 0 {
		return nil, nil
	}

	// 把成员转成 evt key
	evtKeys := make([]string, 0, len(members))
	for _, m := range members {
		evtKeys = append(evtKeys, "recon:evt:"+m)
	}
	vals, err := s.r.MGet(ctx, evtKeys...).Result()
	if err != nil {
		return nil, fmt.Errorf("mget: %w", err)
	}
	out := make(EventList, 0, len(vals))
	for _, raw := range vals {
		if raw == nil {
			continue
		}
		s, ok := raw.(string)
		if !ok || s == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(s), &e); err != nil {
			continue // 单条破坏不影响其他
		}
		out = append(out, &e)
	}
	return out, nil
}

// GetEvent 直接按 (svc, table, pk) 拿一条事件。
func (s *Searcher) GetEvent(ctx context.Context, service, table, pk string) (*Event, error) {
	key := fmt.Sprintf("recon:evt:%s:%s:%s", service, table, pk)
	raw, err := s.r.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &e, nil
}

// ScanIndex 遍历某个 idx_name 下所有 value（编辑器搜索框模糊补全 / 脚本扫一段时间用）。
//
// 实现：SCAN cursor MATCH "recon:idx:<idx>:<prefix>*" COUNT 1000，去掉前缀返回 value 列表。
// 上限 limit 防爆：超过就早返。
func (s *Searcher) ScanIndex(ctx context.Context, idxName, prefix string, limit int) ([]string, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	pattern := fmt.Sprintf("recon:idx:%s:%s*", idxName, prefix)
	stripPrefix := fmt.Sprintf("recon:idx:%s:", idxName)
	var (
		cursor uint64
		out    []string
	)
	for {
		var keys []string
		var err error
		keys, cursor, err = s.r.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return out, err
		}
		for _, k := range keys {
			out = append(out, strings.TrimPrefix(k, stripPrefix))
			if len(out) >= limit {
				return out, nil
			}
		}
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

// ListSchemas 给编辑器自动补全用：列出所有同步过的表。
func (s *Searcher) ListSchemas(ctx context.Context) ([]string, error) {
	return s.r.SMembers(ctx, "recon:meta:tables").Result()
}

// GetSchema 拿一张表的列定义（编辑器右侧 detail panel 用）。
func (s *Searcher) GetSchema(ctx context.Context, service, table string) (string, error) {
	key := fmt.Sprintf("recon:meta:schema:%s:%s", service, table)
	return s.r.Get(ctx, key).Result()
}

// ListIndexKeys 编辑器可选的索引列表（order_id / pi_id / ...）。
func (s *Searcher) ListIndexKeys(ctx context.Context) ([]string, error) {
	return s.r.SMembers(ctx, "recon:meta:idx_keys").Result()
}

// SMembersByIndex 暴露给 JoinByIndex：取索引 SET 的原始成员列表
// （未做主存批量查询，避免无谓 N 次 GET）。
func (s *Searcher) SMembersByIndex(ctx context.Context, idxName, value string) ([]string, error) {
	return s.r.SMembers(ctx, fmt.Sprintf("recon:idx:%s:%s", idxName, value)).Result()
}

// GetEventsByRefs 按 "<svc>:<table>:<pk>" 列表批量取主存。一个 MGET 拿完。
// 用于 JoinByIndex 求交集后的批量加载。
func (s *Searcher) GetEventsByRefs(ctx context.Context, refs []string) (EventList, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	keys := make([]string, len(refs))
	for i, r := range refs {
		keys[i] = "recon:evt:" + r
	}
	vals, err := s.r.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make(EventList, 0, len(vals))
	for _, raw := range vals {
		if raw == nil {
			continue
		}
		s, ok := raw.(string)
		if !ok || s == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(s), &e); err != nil {
			continue
		}
		out = append(out, &e)
	}
	return out, nil
}
