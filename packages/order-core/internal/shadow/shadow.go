// Package shadow 是 github.com/xiongwp/payment-util/shadow 的过渡薄壳。
//
// 真实实现已经搬到 payment-util，让所有 14 个仓库共享同一份 ctx helper /
// metadata 契约 / interceptor。本包仅做 re-export，保持 order-core 现有 import
// 路径（github.com/xiongwp/order-core/internal/shadow）不被破坏。
//
// 新代码请直接 import github.com/xiongwp/payment-util/shadow。
package shadow

import (
	pshadow "github.com/xiongwp/payment-util/shadow"
)

// 常量 alias
const (
	MetadataKey = pshadow.MetadataKey
	Suffix      = pshadow.Suffix
)

// 函数变量 alias
var (
	WithShadow             = pshadow.WithShadow
	IsShadow               = pshadow.IsShadow
	TableName              = pshadow.TableName
	RedisKey               = pshadow.RedisKey
	KafkaTopic             = pshadow.KafkaTopic
	// FromMetadata / UnaryServerInterceptor / UnaryClientInterceptor 已从
	// payment-util/shadow 删除 (Kitex 切换). Kitex MW 由 kitexutil 提供, 不再
	// 走 gRPC interceptor.
	HTTPHeaderToContext = pshadow.HTTPHeaderToContext
	WithoutCancel          = pshadow.WithoutCancel

	// 通用 ID 按位编码 helper alias
	EncodeID    = pshadow.EncodeID
	EncodeIDStr = pshadow.EncodeIDStr
	DecodeID    = pshadow.DecodeID
)

// IDType 常量 alias（按系统分段，详见 payment-util/shadow/identity.go）
const (
	IDTypeOrderPI               = pshadow.IDTypeOrderPI
	IDTypeOrderCharge           = pshadow.IDTypeOrderCharge
	IDTypeOrderRefund           = pshadow.IDTypeOrderRefund
	IDTypeOrderGLTxn            = pshadow.IDTypeOrderGLTxn
	IDTypeOrderDispute          = pshadow.IDTypeOrderDispute
	IDTypeOrderInboundWebhook   = pshadow.IDTypeOrderInboundWebhook
	IDTypeOrderAccountingOutbox = pshadow.IDTypeOrderAccountingOutbox
	IDTypeOrderKYCDoc           = pshadow.IDTypeOrderKYCDoc
	IDTypeOrderEvtTest          = pshadow.IDTypeOrderEvtTest
)

// 类型 alias（让外部声明 shadow.HeaderGetter 等仍可用）
type HeaderGetter = pshadow.HeaderGetter
