package channel

import (
	"context"
	"time"
)

// ClientType 客户端类型（服务端根据这个选择通知手段）
type ClientType string

const (
	ClientTypeUnknown   ClientType = ""
	ClientTypeWeb       ClientType = "web"       // 浏览器 H5，通常走 webhook + 前端轮询
	ClientTypeIOS       ClientType = "ios"       // 走 APNs push
	ClientTypeAndroid   ClientType = "android"   // 走 FCM push
	ClientTypeMiniApp   ClientType = "miniapp"   // 小程序 / 公众号，走订阅消息
	ClientTypeServer    ClientType = "server"    // 商户服务端，走 HTTP webhook 回调
	ClientTypeInternal  ClientType = "internal"  // 平台内部系统，走 MQ / RPC
)

// NotifyChannel 通知渠道
type NotifyChannel string

const (
	NotifyChannelHTTP      NotifyChannel = "http"       // HTTPS POST 回调商户 notify_url（server 端）
	NotifyChannelAPNs      NotifyChannel = "apns"       // Apple Push Notification service（iOS）
	NotifyChannelFCM       NotifyChannel = "fcm"        // Firebase Cloud Messaging（Android）
	NotifyChannelWebsocket NotifyChannel = "websocket"  // 前端建立的长连接（Web / 小程序）
	NotifyChannelMQ        NotifyChannel = "mq"         // 内部 Kafka / NSQ / RabbitMQ
	NotifyChannelSMS       NotifyChannel = "sms"        // 降级手段：短信
	NotifyChannelEmail     NotifyChannel = "email"      // 降级手段：邮件
)

// NotifyRequest 通知请求
type NotifyRequest struct {
	// EventID 事件唯一 ID（幂等）
	EventID string
	// EventType payment_intent.succeeded / payment_intent.requires_action / charge.refunded / ...
	EventType string
	// PaymentIntentID / ChargeID / RefundID 其中之一必填（取决于 EventType）
	PaymentIntentID string
	ChargeID        string
	RefundID        string
	// Target 接收方身份：商户 server 用 URL；设备推送用 device_token；WebSocket 用 session_id
	Target string
	// Payload JSON-serializable body 给接收端展示
	Payload []byte
	Headers map[string]string
	Timeout time.Duration
}

// NotifyResult 单次通知结果
type NotifyResult struct {
	// Success 是否成功触达（HTTP 2xx / 推送成功等）
	Success bool
	// HTTPStatus / MessageID / ErrorCode 具体 driver 填写
	HTTPStatus int
	MessageID  string
	ErrorCode  string
	ErrorMsg   string
	// LatencyMs 耗时
	LatencyMs int64
	// Raw 原始响应 / 失败明细（供审计）
	Raw map[string]string
}

// Notifier 通知发送接口。每种 NotifyChannel 注册一份实现。
//
// 示例：
//
//	HTTPNotifier         POST 商户 notify_url（server 回调）
//	APNsNotifier         Apple 推送
//	FCMNotifier          Firebase / Google 推送
//	WebsocketNotifier    向已建立的 WS session 推送
//	KafkaNotifier        内部 MQ 通知
type Notifier interface {
	Channel() NotifyChannel
	Send(ctx context.Context, req NotifyRequest) (*NotifyResult, error)
}

// NotifyRouter 按 ClientType 决定用哪种 NotifyChannel。
// 支持多渠道 fallback（如 iOS 先 APNs，失败再走 HTTP）。
type NotifyRouter interface {
	// Route 返回按优先级排列的通知渠道列表
	Route(clientType ClientType) []NotifyChannel
	// Register 自定义路由规则
	Register(clientType ClientType, channels ...NotifyChannel)
}
