// typealias.go — 把 kitex_gen 里的 proto 类型 alias 到本包.
//
// 历史: admin_handlers.go 用 ListGraphsRequest / Graph / GraphSummary 等未限定
// 名字, 这套是原 gRPC 时代 server 跟 proto 同包的遗留 import 形态. 切 Kitex 后
// 生成代码挪到 kitex_gen/split_payment/v1/, 这层 alias 让 handler 文件零改动.
//
// 依赖: kitex_gen/ 必须先用 kitex CLI 生成, 见 api/proto/split_payment/v1/admin.proto.
//   kitex -module github.com/xiongwp/split-payment -type protobuf \
//         api/proto/split_payment/v1/admin.proto
package grpcsvc

import (
	splitv1 "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1"
)

// 数据 message
type (
	Graph        = splitv1.Graph
	GraphSummary = splitv1.GraphSummary
	TxnVoucher   = splitv1.TxnVoucher
)

// Request / Response
type (
	ListGraphsRequest    = splitv1.ListGraphsRequest
	ListGraphsResponse   = splitv1.ListGraphsResponse
	GetGraphRequest      = splitv1.GetGraphRequest
	GetGraphResponse     = splitv1.GetGraphResponse
	SaveGraphRequest     = splitv1.SaveGraphRequest
	SaveGraphResponse    = splitv1.SaveGraphResponse
	DeleteGraphRequest   = splitv1.DeleteGraphRequest
	DeleteGraphResponse  = splitv1.DeleteGraphResponse
	DryRunRequest        = splitv1.DryRunRequest
	DryRunResponse       = splitv1.DryRunResponse
	TriggerEventRequest  = splitv1.TriggerEventRequest
	TriggerEventResponse = splitv1.TriggerEventResponse
)
