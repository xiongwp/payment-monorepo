package channel

import "sync"

// DefaultNotifyRouter 基于 map 的默认路由实现，支持 fallback 链。
// 默认规则：
//
//	server   → [http]
//	web      → [websocket, http]
//	ios      → [apns,  http]
//	android  → [fcm,   http]
//	miniapp  → [websocket, http]
//	internal → [mq]
//
// 业务可在启动期用 Register 覆盖。
type DefaultNotifyRouter struct {
	mu    sync.RWMutex
	rules map[ClientType][]NotifyChannel
}

// NewDefaultNotifyRouter 构造并填充默认规则
func NewDefaultNotifyRouter() *DefaultNotifyRouter {
	return &DefaultNotifyRouter{rules: map[ClientType][]NotifyChannel{
		ClientTypeServer:   {NotifyChannelHTTP},
		ClientTypeWeb:      {NotifyChannelWebsocket, NotifyChannelHTTP},
		ClientTypeIOS:      {NotifyChannelAPNs, NotifyChannelHTTP},
		ClientTypeAndroid:  {NotifyChannelFCM, NotifyChannelHTTP},
		ClientTypeMiniApp:  {NotifyChannelWebsocket, NotifyChannelHTTP},
		ClientTypeInternal: {NotifyChannelMQ},
	}}
}

// Route 返回按优先级排列的渠道链；未注册时回退到 HTTP
func (r *DefaultNotifyRouter) Route(ct ClientType) []NotifyChannel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if chs, ok := r.rules[ct]; ok {
		out := make([]NotifyChannel, len(chs))
		copy(out, chs)
		return out
	}
	return []NotifyChannel{NotifyChannelHTTP}
}

// Register 覆盖 / 新增路由规则
func (r *DefaultNotifyRouter) Register(ct ClientType, chs ...NotifyChannel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NotifyChannel, len(chs))
	copy(out, chs)
	r.rules[ct] = out
}
