package model

import "time"

// BufferFlushLevel 缓冲记账刷新间隔等级（分钟）
type BufferFlushLevel int

const (
	BufferFlushLevel1Min   BufferFlushLevel = 1    // 每 1 分钟刷新
	BufferFlushLevel5Min   BufferFlushLevel = 5    // 每 5 分钟刷新
	BufferFlushLevel10Min  BufferFlushLevel = 10   // 每 10 分钟刷新
	BufferFlushLevel60Min  BufferFlushLevel = 60   // 每 60 分钟刷新
	BufferFlushLevel24Hour BufferFlushLevel = 1440 // 每 24 小时刷新
)

// AllBufferFlushLevels 所有合法等级（按间隔升序）
var AllBufferFlushLevels = []BufferFlushLevel{
	BufferFlushLevel1Min,
	BufferFlushLevel5Min,
	BufferFlushLevel10Min,
	BufferFlushLevel60Min,
	BufferFlushLevel24Hour,
}

// BufferAccountConfig 缓冲记账账户配置
// 存于 account_meta.buffer_account_config，服务启动时加载到内存，
// 按 flush_interval_level 分组调度后台刷新任务。
//
// JSON tags 采用 snake_case 以匹配 admin-web 前端约定（没有 tag 时默认用
// 字段名 ID/AccountNo/... 会让前端读不到字段，列表渲染为空）。
type BufferAccountConfig struct {
	ID                 int64            `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	AccountNo          string           `gorm:"column:account_no"                  json:"account_no"`
	FlushIntervalLevel BufferFlushLevel `gorm:"column:flush_interval_level"        json:"flush_interval_level"`
	Enabled            bool             `gorm:"column:enabled"                     json:"enabled"`
	Description        string           `gorm:"column:description"                 json:"description"`
	CreatedAt          time.Time        `gorm:"column:created_at;autoCreateTime"   json:"created_at"`
	UpdatedAt          time.Time        `gorm:"column:updated_at;autoUpdateTime"   json:"updated_at"`
}

func (BufferAccountConfig) TableName() string { return "buffer_account_config" }
