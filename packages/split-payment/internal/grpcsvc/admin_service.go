// admin_service.go — split-payment hand-written gRPC AdminService.
//
// 目的: split-payment 转为纯 gRPC 内部服务 (HTTP server 已删除).
// admin-web BFF 通过本 service 拿 Graph + 跑 dry-run; 业务事件触发的 engine 工作流
// 通过 Kafka 订阅, 跟 gRPC 端正交.
//
// 设计取舍:
//   - graph spec / event / plan 体量大且字段多, 用 bytes spec_json 承载 JSON 字面量,
//     不在 proto 里复刻 domain.GraphSpec 整棵树 (那要 50+ 个 message 类型).
//   - 这跟 accounting-system TransactionService 那批用 typed leg 不同 — 因为这里
//     的数据流是 admin UI ↔ split-payment, 透明传 JSON 即可; 业务原子性由 dry-run
//     回包的 plan_json 体现.
//
// Wire:
//   service: split_payment.v1.AdminService
//   methods:
//     ListGraphs (no params)        → []GraphSummary
//     GetGraph   (key)              → Graph (含 spec_json)
//     SaveGraph  (Graph)            → {key, version}
//     DeleteGraph(key)              → {}
//     DryRun     (graph, event)     → plan_json
package grpcsvc

import (
	"context"
	"fmt"

	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// ─── wire types ────────────────────────────────────────────────────────────

// GraphSummary 列表项 (不含 spec, 只给 dropdown 用).
type GraphSummary struct {
	Key       string `protobuf:"bytes,1,opt,name=key,proto3" json:"key,omitempty"`
	Name      string `protobuf:"bytes,2,opt,name=name,proto3" json:"name,omitempty"`
	Version   string `protobuf:"bytes,3,opt,name=version,proto3" json:"version,omitempty"`
	Status    string `protobuf:"bytes,4,opt,name=status,proto3" json:"status,omitempty"`
	OwnerType string `protobuf:"bytes,5,opt,name=owner_type,json=ownerType,proto3" json:"owner_type,omitempty"`
	OwnerID   string `protobuf:"bytes,6,opt,name=owner_id,json=ownerId,proto3" json:"owner_id,omitempty"`
}

func (x *GraphSummary) Reset()         { *x = GraphSummary{} }
func (x *GraphSummary) String() string { return fmt.Sprintf("%+v", *x) }
func (*GraphSummary) ProtoMessage()    {}

// Graph 完整结构 (含 spec_json).
type Graph struct {
	Key       string `protobuf:"bytes,1,opt,name=key,proto3" json:"key,omitempty"`
	Name      string `protobuf:"bytes,2,opt,name=name,proto3" json:"name,omitempty"`
	Version   string `protobuf:"bytes,3,opt,name=version,proto3" json:"version,omitempty"`
	Status    string `protobuf:"bytes,4,opt,name=status,proto3" json:"status,omitempty"`
	OwnerType string `protobuf:"bytes,5,opt,name=owner_type,json=ownerType,proto3" json:"owner_type,omitempty"`
	OwnerID   string `protobuf:"bytes,6,opt,name=owner_id,json=ownerId,proto3" json:"owner_id,omitempty"`
	// SpecJson 是 domain.GraphSpec 的 JSON 字节. 调用方用 json.Marshal/Unmarshal 即可.
	SpecJson []byte `protobuf:"bytes,7,opt,name=spec_json,json=specJson,proto3" json:"spec_json,omitempty"`
}

func (x *Graph) Reset()         { *x = Graph{} }
func (x *Graph) String() string { return fmt.Sprintf("%+v", *x) }
func (*Graph) ProtoMessage()    {}

// ListGraphsRequest.
type ListGraphsRequest struct{}

func (x *ListGraphsRequest) Reset()         { *x = ListGraphsRequest{} }
func (x *ListGraphsRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListGraphsRequest) ProtoMessage()    {}

// ListGraphsResponse.
type ListGraphsResponse struct {
	Items []*GraphSummary `protobuf:"bytes,1,rep,name=items,proto3" json:"items,omitempty"`
}

func (x *ListGraphsResponse) Reset()         { *x = ListGraphsResponse{} }
func (x *ListGraphsResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListGraphsResponse) ProtoMessage()    {}

// GetGraphRequest.
type GetGraphRequest struct {
	Key string `protobuf:"bytes,1,opt,name=key,proto3" json:"key,omitempty"`
}

func (x *GetGraphRequest) Reset()         { *x = GetGraphRequest{} }
func (x *GetGraphRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*GetGraphRequest) ProtoMessage()    {}

// GetGraphResponse.
type GetGraphResponse struct {
	Graph *Graph `protobuf:"bytes,1,opt,name=graph,proto3" json:"graph,omitempty"`
}

func (x *GetGraphResponse) Reset()         { *x = GetGraphResponse{} }
func (x *GetGraphResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*GetGraphResponse) ProtoMessage()    {}

// SaveGraphRequest.
type SaveGraphRequest struct {
	Graph *Graph `protobuf:"bytes,1,opt,name=graph,proto3" json:"graph,omitempty"`
}

func (x *SaveGraphRequest) Reset()         { *x = SaveGraphRequest{} }
func (x *SaveGraphRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*SaveGraphRequest) ProtoMessage()    {}

// SaveGraphResponse.
type SaveGraphResponse struct {
	Key     string `protobuf:"bytes,1,opt,name=key,proto3" json:"key,omitempty"`
	Version string `protobuf:"bytes,2,opt,name=version,proto3" json:"version,omitempty"`
}

func (x *SaveGraphResponse) Reset()         { *x = SaveGraphResponse{} }
func (x *SaveGraphResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*SaveGraphResponse) ProtoMessage()    {}

// DeleteGraphRequest.
type DeleteGraphRequest struct {
	Key string `protobuf:"bytes,1,opt,name=key,proto3" json:"key,omitempty"`
}

func (x *DeleteGraphRequest) Reset()         { *x = DeleteGraphRequest{} }
func (x *DeleteGraphRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*DeleteGraphRequest) ProtoMessage()    {}

// DeleteGraphResponse.
type DeleteGraphResponse struct{}

func (x *DeleteGraphResponse) Reset()         { *x = DeleteGraphResponse{} }
func (x *DeleteGraphResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*DeleteGraphResponse) ProtoMessage()    {}

// DryRunRequest — graph + 触发事件 → 翻译输出预览 (不落账).
type DryRunRequest struct {
	Graph *Graph `protobuf:"bytes,1,opt,name=graph,proto3" json:"graph,omitempty"`
	// EventJson = workflow.TriggerContext 的 JSON, caller 自己组装.
	EventJson []byte `protobuf:"bytes,2,opt,name=event_json,json=eventJson,proto3" json:"event_json,omitempty"`
}

func (x *DryRunRequest) Reset()         { *x = DryRunRequest{} }
func (x *DryRunRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*DryRunRequest) ProtoMessage()    {}

// DryRunResponse — PlanJson 是 *domain.RunPlan 的 JSON (含 Transactions 等).
type DryRunResponse struct {
	PlanJson []byte `protobuf:"bytes,1,opt,name=plan_json,json=planJson,proto3" json:"plan_json,omitempty"`
	Error    string `protobuf:"bytes,2,opt,name=error,proto3" json:"error,omitempty"`
}

func (x *DryRunResponse) Reset()         { *x = DryRunResponse{} }
func (x *DryRunResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*DryRunResponse) ProtoMessage()    {}

// ─── Server interface ──────────────────────────────────────────────────────

type AdminServiceServer interface {
	ListGraphs(context.Context, *ListGraphsRequest) (*ListGraphsResponse, error)
	GetGraph(context.Context, *GetGraphRequest) (*GetGraphResponse, error)
	SaveGraph(context.Context, *SaveGraphRequest) (*SaveGraphResponse, error)
	DeleteGraph(context.Context, *DeleteGraphRequest) (*DeleteGraphResponse, error)
	DryRun(context.Context, *DryRunRequest) (*DryRunResponse, error)
	mustEmbedUnimplementedAdminServiceServer()
}

// UnimplementedAdminServiceServer 默认 not-implemented.
type UnimplementedAdminServiceServer struct{}

func (UnimplementedAdminServiceServer) ListGraphs(context.Context, *ListGraphsRequest) (*ListGraphsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "ListGraphs not implemented")
}
func (UnimplementedAdminServiceServer) GetGraph(context.Context, *GetGraphRequest) (*GetGraphResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "GetGraph not implemented")
}
func (UnimplementedAdminServiceServer) SaveGraph(context.Context, *SaveGraphRequest) (*SaveGraphResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "SaveGraph not implemented")
}
func (UnimplementedAdminServiceServer) DeleteGraph(context.Context, *DeleteGraphRequest) (*DeleteGraphResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "DeleteGraph not implemented")
}
func (UnimplementedAdminServiceServer) DryRun(context.Context, *DryRunRequest) (*DryRunResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "DryRun not implemented")
}
func (UnimplementedAdminServiceServer) mustEmbedUnimplementedAdminServiceServer() {}

// ─── Client interface ──────────────────────────────────────────────────────

type AdminServiceClient interface {
	ListGraphs(ctx context.Context, in *ListGraphsRequest, opts ...grpc.CallOption) (*ListGraphsResponse, error)
	GetGraph(ctx context.Context, in *GetGraphRequest, opts ...grpc.CallOption) (*GetGraphResponse, error)
	SaveGraph(ctx context.Context, in *SaveGraphRequest, opts ...grpc.CallOption) (*SaveGraphResponse, error)
	DeleteGraph(ctx context.Context, in *DeleteGraphRequest, opts ...grpc.CallOption) (*DeleteGraphResponse, error)
	DryRun(ctx context.Context, in *DryRunRequest, opts ...grpc.CallOption) (*DryRunResponse, error)
}

type adminServiceClient struct{ cc grpc.ClientConnInterface }

func NewAdminServiceClient(cc grpc.ClientConnInterface) AdminServiceClient {
	return &adminServiceClient{cc}
}

func (c *adminServiceClient) ListGraphs(ctx context.Context, in *ListGraphsRequest, opts ...grpc.CallOption) (*ListGraphsResponse, error) {
	out := new(ListGraphsResponse)
	if err := c.cc.Invoke(ctx, "/split_payment.v1.AdminService/ListGraphs", in, out, append([]grpc.CallOption{grpc.StaticMethod()}, opts...)...); err != nil {
		return nil, err
	}
	return out, nil
}
func (c *adminServiceClient) GetGraph(ctx context.Context, in *GetGraphRequest, opts ...grpc.CallOption) (*GetGraphResponse, error) {
	out := new(GetGraphResponse)
	if err := c.cc.Invoke(ctx, "/split_payment.v1.AdminService/GetGraph", in, out, append([]grpc.CallOption{grpc.StaticMethod()}, opts...)...); err != nil {
		return nil, err
	}
	return out, nil
}
func (c *adminServiceClient) SaveGraph(ctx context.Context, in *SaveGraphRequest, opts ...grpc.CallOption) (*SaveGraphResponse, error) {
	out := new(SaveGraphResponse)
	if err := c.cc.Invoke(ctx, "/split_payment.v1.AdminService/SaveGraph", in, out, append([]grpc.CallOption{grpc.StaticMethod()}, opts...)...); err != nil {
		return nil, err
	}
	return out, nil
}
func (c *adminServiceClient) DeleteGraph(ctx context.Context, in *DeleteGraphRequest, opts ...grpc.CallOption) (*DeleteGraphResponse, error) {
	out := new(DeleteGraphResponse)
	if err := c.cc.Invoke(ctx, "/split_payment.v1.AdminService/DeleteGraph", in, out, append([]grpc.CallOption{grpc.StaticMethod()}, opts...)...); err != nil {
		return nil, err
	}
	return out, nil
}
func (c *adminServiceClient) DryRun(ctx context.Context, in *DryRunRequest, opts ...grpc.CallOption) (*DryRunResponse, error) {
	out := new(DryRunResponse)
	if err := c.cc.Invoke(ctx, "/split_payment.v1.AdminService/DryRun", in, out, append([]grpc.CallOption{grpc.StaticMethod()}, opts...)...); err != nil {
		return nil, err
	}
	return out, nil
}

// ─── Handlers + ServiceDesc ────────────────────────────────────────────────

func _AdminService_ListGraphs_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ListGraphsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AdminServiceServer).ListGraphs(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/split_payment.v1.AdminService/ListGraphs"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AdminServiceServer).ListGraphs(ctx, req.(*ListGraphsRequest))
	}
	return interceptor(ctx, in, info, handler)
}
func _AdminService_GetGraph_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(GetGraphRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AdminServiceServer).GetGraph(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/split_payment.v1.AdminService/GetGraph"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AdminServiceServer).GetGraph(ctx, req.(*GetGraphRequest))
	}
	return interceptor(ctx, in, info, handler)
}
func _AdminService_SaveGraph_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(SaveGraphRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AdminServiceServer).SaveGraph(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/split_payment.v1.AdminService/SaveGraph"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AdminServiceServer).SaveGraph(ctx, req.(*SaveGraphRequest))
	}
	return interceptor(ctx, in, info, handler)
}
func _AdminService_DeleteGraph_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(DeleteGraphRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AdminServiceServer).DeleteGraph(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/split_payment.v1.AdminService/DeleteGraph"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AdminServiceServer).DeleteGraph(ctx, req.(*DeleteGraphRequest))
	}
	return interceptor(ctx, in, info, handler)
}
func _AdminService_DryRun_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(DryRunRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AdminServiceServer).DryRun(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/split_payment.v1.AdminService/DryRun"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AdminServiceServer).DryRun(ctx, req.(*DryRunRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// AdminService_ServiceDesc.
var AdminService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "split_payment.v1.AdminService",
	HandlerType: (*AdminServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "ListGraphs", Handler: _AdminService_ListGraphs_Handler},
		{MethodName: "GetGraph", Handler: _AdminService_GetGraph_Handler},
		{MethodName: "SaveGraph", Handler: _AdminService_SaveGraph_Handler},
		{MethodName: "DeleteGraph", Handler: _AdminService_DeleteGraph_Handler},
		{MethodName: "DryRun", Handler: _AdminService_DryRun_Handler},
	},
	Streams: []grpc.StreamDesc{},
}

// RegisterAdminServiceServer.
func RegisterAdminServiceServer(s grpc.ServiceRegistrar, srv AdminServiceServer) {
	s.RegisterService(&AdminService_ServiceDesc, srv)
}
