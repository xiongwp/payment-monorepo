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

// Source 一个 service 的 binlog 订阅源 + 关心的表 + 关心的列。
type Source struct {
	Service  string `yaml:"service"`   // "order-core"
	Addr     string `yaml:"addr"`      // "host:3306,host2:3306"  支持多分片实例
	User     string `yaml:"user"`      // 用 REPLICATION CLIENT/SLAVE 权限的专用账号
	Password string `yaml:"password"`  // 走 env 替换（${RECON_MYSQL_PASS}）
	ServerID uint32 `yaml:"server_id"` // canal 用，全局唯一
	Schemas  []string `yaml:"schemas"` // shard 库列表（可省略 → 自动发现）
	Tables   map[string]TableConfig `yaml:"tables"`
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
