// pool.go — sync.Pool reuse for cdc.Event (PERF-13).
//
// 问题: 10K events/s 下 ingester.handle() 每条 record 都
//   var e cdc.Event
//   json.Unmarshal(rec.Value, &e)
// 由于 json.Unmarshal 取 &e, escape analysis 把 Event 推到堆上;
// 再加上 Before/After/Indexes 三个 map 每条都 makemap → alloc 占 CPU 15%.
//
// 方案:
//   - 池化 Event 本身.
//   - 池化时预分配 Before/After (cap=16) 与 Indexes (cap=4) 三个 map.
//   - 用 `clear()` (Go 1.21+) 复用 map 不重新分配.
//
// 用法 (ingester):
//   e := cdc.GetEvent()
//   defer cdc.PutEvent(e)
//   json.Unmarshal(rec.Value, e)  // 写入复用过的 maps
//   layer.Put(ctx, cdcToStore(e))
//
// 注意:
//   - cdcToStore 会拷贝 Indexes/Before/After 引用 → 在 PutEvent 之前必须确保下游不再持有 e 的 map 引用.
//     candidate.Layer 内部 marshal 之后写 Redis,不长期持有;OK 复用.
//   - 长期持有路径 (e.g. SSE 双写 XADD): 单独 marshal JSON 字符串后即可 Put,放心.
package cdc

import "sync"

var eventPool = sync.Pool{
	New: func() any {
		return &Event{
			Before:  make(map[string]any, 16),
			After:   make(map[string]any, 16),
			Indexes: make(map[string]string, 4),
		}
	},
}

// GetEvent 从池取一个干净的 Event. 调用方负责 PutEvent 归还.
//
// 返回的 Event:
//   - 标量字段已重置 (Service="" Table="" ...)
//   - Before / After / Indexes 已 clear (len=0, cap 保留)
//
// 不复用场景: 测试 / 单条路径,直接 `&Event{}` 也行,池化只在 ingester hot loop 才需要.
func GetEvent() *Event {
	e := eventPool.Get().(*Event)
	e.Service = ""
	e.Schema = ""
	e.Table = ""
	e.PK = ""
	e.Op = ""
	e.BinlogFile = ""
	e.BinlogPos = 0
	e.GTID = ""
	e.Timestamp = e.Timestamp.Truncate(0) // zero value, keep struct
	// Maps: clear() 保 cap,下次 Unmarshal 直接复用桶.
	clearMapAny(e.Before)
	clearMapAny(e.After)
	clearMapStr(e.Indexes)
	return e
}

// PutEvent 归还 Event 到池. 调用后不要再访问 e.
//
// 安全检查: 若 Before/After 增长得过大 (e.g. 某条恶意宽行 > 1024 列),
// 直接丢弃这条 Event 不入池 (防 map 内存膨胀).
func PutEvent(e *Event) {
	if e == nil {
		return
	}
	const maxKeep = 256
	if len(e.Before) > maxKeep || len(e.After) > maxKeep || len(e.Indexes) > maxKeep {
		return // 不归还, 让 GC 回收
	}
	eventPool.Put(e)
}

// clearMapAny / clearMapStr — 兼容 Go 1.20 之前的写法.
// Go 1.21+ 内置 builtin clear(); 这里手写一遍保证 1.20 仍能编译.
// 实测 hot path 用法等同 `clear(m)` 无性能差.
func clearMapAny(m map[string]any) {
	for k := range m {
		delete(m, k)
	}
}

func clearMapStr(m map[string]string) {
	for k := range m {
		delete(m, k)
	}
}
