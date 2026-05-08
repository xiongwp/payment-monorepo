// Package cdc 把业务服务 MySQL binlog 实时摄入 reconplatform Redis。
//
// 数据流：
//
//	业务库 binlog (ROW)
//	    ↓ go-mysql-org/go-mysql/canal
//	cdc.Canal.OnRow → Event{svc, table, op, pk, before, after}
//	    ↓
//	cdc.Publisher → Redis Pipeline:
//	    SET   recon:evt:<svc>:<table>:<pk>     JSON
//	    SADD  recon:idx:<idx_name>:<value>     "<svc>:<table>:<pk>"  ×N
//	    XADD  recon:stream:events              触发事件驱动脚本
//	    HSET  recon:cdc:pos:<svc>              file/pos/gtid   (异步 fsync)
//
// 每条业务 key 索引带回写：order_id / pi_id / transaction_id / merchant_id 等
// 由 source 配置声明，便于跨服务关联查询。
package cdc

import (
	"encoding/json"
	"time"
)

// Op binlog 行操作类型。
type Op string

const (
	OpInsert Op = "INSERT"
	OpUpdate Op = "UPDATE"
	OpDelete Op = "DELETE"
)

// Event 单条 row 变更事件，发布到 Redis 之前由 parser 构造。
type Event struct {
	// 标识
	Service string `json:"svc"`   // "order-core"
	Schema  string `json:"db"`    // "order_core_db_5"  shard 库名
	Table   string `json:"table"` // "payment_intents"
	PK      string `json:"pk"`    // 主键值（多列拼接 "v1|v2"）
	Op      Op     `json:"op"`    // INSERT / UPDATE / DELETE

	// 行数据：UPDATE 时 Before 是旧值，After 是新值；INSERT 只 After；DELETE 只 Before
	Before map[string]any `json:"before,omitempty"`
	After  map[string]any `json:"after,omitempty"`

	// binlog 元信息（用于 crash recovery + 排序去重）
	BinlogFile string    `json:"binlog_file"`
	BinlogPos  uint32    `json:"binlog_pos"`
	GTID       string    `json:"gtid,omitempty"`
	Timestamp  time.Time `json:"ts"`

	// 索引值：source 配置指定的列在本事件 row 里的实际值。
	// 例：{"pi_id": "pi_xxx", "merchant_id": "m_yyy"}
	// publisher 用这个跑 SADD recon:idx:<k>:<v> → "<svc>:<table>:<pk>"
	Indexes map[string]string `json:"indexes,omitempty"`
}

// JSON 序列化用，store 写 Redis 用。失败时返 "{}"（不阻塞 publish）。
func (e *Event) JSON() string {
	b, err := json.Marshal(e)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// EventKey 主存 Redis key。
func (e *Event) EventKey() string {
	return "recon:evt:" + e.Service + ":" + e.Table + ":" + e.PK
}

// MemberRef 索引 SET 里的成员，用于反查时定位主存 key。
func (e *Event) MemberRef() string {
	return e.Service + ":" + e.Table + ":" + e.PK
}

// Row 取最新一份行数据：UPDATE 用 After，INSERT 用 After，DELETE 用 Before。
// 脚本 ctx.GetByIndex 返回的 Event 上调用此方法。
func (e *Event) Row() map[string]any {
	if len(e.After) > 0 {
		return e.After
	}
	return e.Before
}
