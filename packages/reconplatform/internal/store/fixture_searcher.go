// fixture_searcher.go — in-memory Searcher 用于 CLI / 单测。
//
// 生产 Searcher 走 Redis;本类型走 map,接口完全一致。
//
// 用法:
//
//	fs := store.NewFixtureSearcher([]store.Event{
//	    {Service: "payment-channel", Table: "acquirer_tx", PK: "tx_1", After: ...},
//	    ...
//	})
//	ctx := script.NewContext(ctx, fs, logger, nil)
//	diffs, _ := engine.Run(ctx, compiled, sctx)
//
// 实现:扁平 slice + 三个索引 (service:table:pk / idx:val→pk / idx→values).
package store

import (
	"context"
	"sort"
	"strings"
)

// FixtureSearcher 内存版 Searcher,实现 script.SearcherIface 的所有方法.
type FixtureSearcher struct {
	// 全部事件 (扁平存)
	events []*Event
	// (svc, table, pk) → 事件
	byKey map[string]*Event
	// idx (e.g. "pi_id") + val (e.g. "pi_xxx") → 全部命中的事件
	byIndex map[string][]*Event
	// idx → 该 idx 下的所有 val
	indexValues map[string]map[string]struct{}
}

// NewFixtureSearcher 构造.
func NewFixtureSearcher(events []Event) *FixtureSearcher {
	fs := &FixtureSearcher{
		byKey:       map[string]*Event{},
		byIndex:     map[string][]*Event{},
		indexValues: map[string]map[string]struct{}{},
	}
	for i := range events {
		e := events[i] // 值拷贝
		fs.add(&e)
	}
	return fs
}

func (fs *FixtureSearcher) add(e *Event) {
	fs.events = append(fs.events, e)
	fs.byKey[e.Service+":"+e.Table+":"+e.PK] = e
	for idx, val := range e.Indexes {
		key := idx + ":" + val
		fs.byIndex[key] = append(fs.byIndex[key], e)
		if fs.indexValues[idx] == nil {
			fs.indexValues[idx] = map[string]struct{}{}
		}
		fs.indexValues[idx][val] = struct{}{}
	}
}

// Add 单独追加一条 (CLI 调用方按需用).
func (fs *FixtureSearcher) Add(e Event) { fs.add(&e) }

// Count 返回当前 fixture 中事件数 (调试用).
func (fs *FixtureSearcher) Count() int { return len(fs.events) }

// SearchByIndex 实现 SearcherIface.
func (fs *FixtureSearcher) SearchByIndex(_ context.Context, idxName, value string) (EventList, error) {
	hits := fs.byIndex[idxName+":"+value]
	if hits == nil {
		return nil, nil
	}
	out := make(EventList, len(hits))
	copy(out, hits)
	return out, nil
}

// GetEvent 实现 SearcherIface.
func (fs *FixtureSearcher) GetEvent(_ context.Context, service, table, pk string) (*Event, error) {
	e, ok := fs.byKey[service+":"+table+":"+pk]
	if !ok {
		return nil, nil
	}
	return e, nil
}

// ScanService 实现 SearcherIface.
func (fs *FixtureSearcher) ScanService(_ context.Context, service, table string, limit int) (EventList, error) {
	var out EventList
	for _, e := range fs.events {
		if e.Service == service && e.Table == table {
			out = append(out, e)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// ScanIndex 实现 SearcherIface.
//
// 返回排序后的 unique values (脚本要顺序稳定才好调试).
func (fs *FixtureSearcher) ScanIndex(_ context.Context, idxName, prefix string, limit int) ([]string, error) {
	vals, ok := fs.indexValues[idxName]
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(vals))
	for v := range vals {
		if prefix == "" || strings.HasPrefix(v, prefix) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
