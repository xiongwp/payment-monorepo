package model

import "time"

// ServiceInstance accounting-system 服务实例注册记录。
// 每个实例启动时向 account_meta.service_instance 注册自身地址，
// 定期心跳续约，停止时将 status 置为 0。
// admin-web 通过查询此表发现所有活跃实例，并向每个实例推送配置变更（热重载）。
type ServiceInstance struct {
	InstanceID    string    `gorm:"column:instance_id;primaryKey"              json:"instance_id"`
	Host          string    `gorm:"column:host"                                json:"host"`
	HTTPAdminPort int       `gorm:"column:http_admin_port"                     json:"http_admin_port"`
	GRPCPort      int       `gorm:"column:grpc_port"                           json:"grpc_port"`
	Status        int8      `gorm:"column:status"                              json:"status"` // 1=alive, 0=stopped
	LastHeartbeat time.Time `gorm:"column:last_heartbeat;autoUpdateTime:milli" json:"last_heartbeat"`
	StartedAt     time.Time `gorm:"column:started_at;autoCreateTime"           json:"started_at"`
}

func (ServiceInstance) TableName() string { return "service_instance" }

const (
	ServiceInstanceStatusAlive   int8 = 1
	ServiceInstanceStatusStopped int8 = 0

	// ServiceInstanceAliveThreshold 超过此时间没有心跳则视为失联实例
	ServiceInstanceAliveThreshold = 60 * time.Second
)
