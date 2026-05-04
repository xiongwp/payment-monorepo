package model

import "time"

// SystemConfig 通用 key-value 配置（meta DB / system_config 表）。
//
// value 用 JSON 字符串持久化，可承载 string / number / bool / array / object。
// value_type 是 admin-web 编辑器的 hint，不是后端校验类型。
//
// 读写流程：
//   - 读：服务启动 + 60s tick + admin 改动后扇出 → 全表 SELECT → 内存 map
//   - 写：admin-web POST /admin/config → upsert → fanout /admin/reload/config
type SystemConfig struct {
	ConfigKey   string    `gorm:"column:config_key;primaryKey" json:"config_key"`
	ValueJSON   string    `gorm:"column:value_json"            json:"value_json"`
	ValueType   string    `gorm:"column:value_type"            json:"value_type"`
	Description string    `gorm:"column:description"           json:"description"`
	UpdatedBy   string    `gorm:"column:updated_by"            json:"updated_by"`
	UpdatedAt   time.Time `gorm:"column:updated_at"            json:"updated_at"`
	CreatedAt   time.Time `gorm:"column:created_at"            json:"created_at"`
}

func (SystemConfig) TableName() string { return "system_config" }
