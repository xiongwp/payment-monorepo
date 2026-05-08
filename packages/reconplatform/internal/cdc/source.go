// Source 一个业务服务的 CDC 摄入配置。整组 sources 是一份 reconplatform
// 全局的"我要订阅哪些服务的哪些表"清单，从 config-center 加载（OnChange 热更）。
//
// 配置示例（config-center key=reconplatform/cdc.sources，YAML 也行）：
//
//	sources:
//	  - service: order-core
//	    addr:    order-core-mysql-0:3306,order-core-mysql-1:3306,...
//	    user:    recon_replicator
//	    password: ${RECON_MYSQL_PASS}
//	    server_id: 100              # 必须全局唯一（每副本 +1）
//	    schemas:
//	      - order_core_db_0
//	      - order_core_db_1
//	      ...
//	    tables:
//	      payment_intents:
//	        pk: [id]
//	        index_columns: [merchant_id, customer_id, user_card_id]
//	      charges:
//	        pk: [id]
//	        index_columns: [pi_id, merchant_id]
//	  - service: accounting-system
//	    ...
package cdc

import "fmt"

// Source 一个 service 的 binlog 订阅配置。
//
// 一个 Service 可能有多个 MySQL 实例（10 分片），每个实例起一个独立 Canal
// channel：
//
//   Addrs:        ["shard-0:3306", "shard-1:3306", ...]
//   ServerIDBase: 1000   → shard-0 用 1000，shard-1 用 1001，...
//
// Schemas 一般和 Addrs 一一对应（shard-0 = order_core_db_0 等），但允许多对多
// （某个 shard 实例可能同时托管多个 schema）。canal 内置 schema 过滤，所以
// 我们传完整 Schemas 列表，canal 自己会忽略本实例没有的 schema。
type Source struct {
	Service       string                 `yaml:"service"`         // "order-core"
	Addrs         []string               `yaml:"addrs"`           // ["shard-0:3306", "shard-1:3306", ...]
	User          string                 `yaml:"user"`            // 用 REPLICATION CLIENT/SLAVE 权限的专用账号
	Password      string                 `yaml:"password"`        // 走 env 替换（${RECON_MYSQL_PASS}）
	ServerIDBase  uint32                 `yaml:"server_id_base"`  // 第 N 个 addr 用 base+N（必须全局唯一）
	Schemas       []string               `yaml:"schemas"`         // shard 库列表（canal 用作过滤）
	Tables        map[string]TableConfig `yaml:"tables"`

	// 旧字段保留兼容（单 addr 场景）：YAML 仍可写 addr/server_id；
	// 启动期 normalize 把它合并进 Addrs/ServerIDBase。
	Addr     string `yaml:"addr,omitempty"`
	ServerID uint32 `yaml:"server_id,omitempty"`
}

// Normalize 兼容老配置（单 addr）+ 校验：
//   - Addr 非空 → 拼到 Addrs 头部
//   - ServerID 非 0 → 当 ServerIDBase
//   - Addrs 仍空 → 报错（caller fail-fast）
func (s *Source) Normalize() error {
	if s.Addr != "" {
		s.Addrs = append([]string{s.Addr}, s.Addrs...)
		s.Addr = ""
	}
	if s.ServerID != 0 && s.ServerIDBase == 0 {
		s.ServerIDBase = s.ServerID
	}
	if len(s.Addrs) == 0 {
		return fmt.Errorf("source %q: at least one addr required", s.Service)
	}
	if s.ServerIDBase == 0 {
		return fmt.Errorf("source %q: server_id_base must be > 0", s.Service)
	}
	return nil
}

// Channels 把多 addr 展开成 N 个 binlog channel（每个用独立 server-id）。
//
// caller (Runner) 启动每个 channel：
//
//   for _, ch := range src.Channels() {
//       go runOneCanal(ch.Addr, ch.ServerID, ...)
//   }
type Channel struct {
	Service  string  // 服务名，所有 channel 共享
	Addr     string  // 实例地址 "host:3306"
	ServerID uint32  // canal 全局唯一 ID
	Index    int     // 在 source.Addrs 中的索引（做日志 / metric 用）
}

func (s *Source) Channels() []Channel {
	out := make([]Channel, 0, len(s.Addrs))
	for i, a := range s.Addrs {
		out = append(out, Channel{
			Service:  s.Service,
			Addr:     a,
			ServerID: s.ServerIDBase + uint32(i),
			Index:    i,
		})
	}
	return out
}

// TableConfig 一张表的关心字段。
type TableConfig struct {
	// PK 主键列名（多列联合主键按顺序拼接：v1|v2）。空时用 information_schema 自动探测。
	PK []string `yaml:"pk"`

	// IndexColumns 这些列的值会写到 recon:idx:<col>:<value> SET 里，给跨服务关联查询用。
	// 例 ["pi_id", "merchant_id"] → 写两个索引；之后脚本可以
	//    ctx.GetByIndex("pi_id", "pi_xxx") 直接拿到所有引用 pi_xxx 的事件。
	IndexColumns []string `yaml:"index_columns"`

	// IgnoreColumns 不希望进 Redis 的列（如 PII、长 blob）。
	IgnoreColumns []string `yaml:"ignore_columns,omitempty"`
}

// SourceList 一份完整配置（所有服务的 sources）。
type SourceList struct {
	Sources []Source `yaml:"sources"`
}

// FullTableName 返回 "<svc>:<table>" 形式，给主存 key / Stream 用。
func (s *Source) FullTableName(table string) string {
	return s.Service + ":" + table
}

// IsSchemaWatched 判断 binlog 里某个 schema 是否在订阅范围内。
// 不指定 Schemas 时全订阅（依靠 Tables 白名单过滤）。
func (s *Source) IsSchemaWatched(schema string) bool {
	if len(s.Schemas) == 0 {
		return true
	}
	for _, ws := range s.Schemas {
		if ws == schema {
			return true
		}
	}
	return false
}
