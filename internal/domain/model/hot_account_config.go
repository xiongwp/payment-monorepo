package model

import "time"

// HotAccountConfig 热点账户配置（Redis 热路径白名单）
// 存于 account_meta.hot_account_config，服务启动时加载到内存。
//
// JSON tags 采用 snake_case 以匹配 accounting-admin-web 前端约定
// （src/types/accounting.ts HotAccountConfig）。没有 tag 时 encoding/json
// 会默认用字段名 ID/AccountNo/...，前端读不到导致新增行各列空白。
type HotAccountConfig struct {
	ID          int64     `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	AccountNo   string    `gorm:"column:account_no"                  json:"account_no"`
	Enabled     bool      `gorm:"column:enabled"                     json:"enabled"`
	Description string    `gorm:"column:description"                 json:"description"`
	CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime"   json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at;autoUpdateTime"   json:"updated_at"`
}

func (HotAccountConfig) TableName() string { return "hot_account_config" }
