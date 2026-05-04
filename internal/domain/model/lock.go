package model

import "time"

// DistributedLock 分布式锁记录（按 lock_key hash 分库分表）
type DistributedLock struct {
	ID         int64     `gorm:"column:id;primaryKey;autoIncrement"`
	LockKey    string    `gorm:"column:lock_key"`
	LockValue  string    `gorm:"column:lock_value"` // UUID，标识持有者
	ExpireTime time.Time `gorm:"column:expire_time"`
	CreatedAt  time.Time `gorm:"column:created_at;autoCreateTime"`
}
