// transaction_service.go — SP-AC-7 hand-written gRPC TransactionService.
//
// 为什么 hand-written 而不走 proto codegen:
//   1. 跟 admin_extensions.go (AccountingAdminService) 同模式, 避免重新 codegen vendored pb
//   2. 同分支内 add-only, 不破坏现有 accountingv1 proto 兼容
//
// 服务:
//   accounting.v1.TransactionService
//
// 方法:
//   CreateTransaction(CreateTransactionRequest) → CreateTransactionResponse
//   ListAccountTypes(ListAccountTypesRequest) → ListAccountTypesResponse
//   ListTransactionRules(ListTransactionRulesRequest) → ListTransactionRulesResponse
//
// 设计:
//   - CreateTransaction 是 multi-leg: 一个 event_code → 一次原子操作, 含 ≥ 1 条 leg.
//     一条 leg = 一对 (from_account_id 借, to_account_id 贷) + amount + currency.
//   - ListAccountTypes / ListTransactionRules 是 read-only 元数据查询,
//     designer picker 间接通过 BFF 调 (BFF 仍走 HTTP /admin/account-types 兼容浏览器).
package grpc

import (
	"context"
	"fmt"

	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// ─── Request / Response 类型 ────────────────────────────────────────────────

// TxnLeg 一条资金流 leg.
//
// 一个 CreateTransactionRequest 含 ≥ 1 条 TxnLeg; 同一请求所有 leg 必须同币种.
type TxnLeg struct {
	EdgeFromNode  string `protobuf:"bytes,1,opt,name=edge_from_node,json=edgeFromNode,proto3" json:"edge_from_node,omitempty"`
	EdgeToNode    string `protobuf:"bytes,2,opt,name=edge_to_node,json=edgeToNode,proto3"     json:"edge_to_node,omitempty"`
	FromAccountId string `protobuf:"bytes,3,opt,name=from_account_id,json=fromAccountId,proto3" json:"from_account_id,omitempty"`
	ToAccountId   string `protobuf:"bytes,4,opt,name=to_account_id,json=toAccountId,proto3"     json:"to_account_id,omitempty"`
	Amount        string `protobuf:"bytes,5,opt,name=amount,proto3"                              json:"amount,omitempty"`
	Currency      string `protobuf:"bytes,6,opt,name=currency,proto3"                            json:"currency,omitempty"`
}

func (x *TxnLeg) Reset()         { *x = TxnLeg{} }
func (x *TxnLeg) String() string { return fmt.Sprintf("%+v", *x) }
func (*TxnLeg) ProtoMessage()    {}

// CreateTransactionRequest 创建一笔多 leg 原子记账.
type CreateTransactionRequest struct {
	OrderNo     string    `protobuf:"bytes,1,opt,name=order_no,json=orderNo,proto3"            json:"order_no,omitempty"`
	ProductCode string    `protobuf:"bytes,2,opt,name=product_code,json=productCode,proto3"    json:"product_code,omitempty"`
	EventCode   string    `protobuf:"bytes,3,opt,name=event_code,json=eventCode,proto3"        json:"event_code,omitempty"`
	Legs        []*TxnLeg `protobuf:"bytes,4,rep,name=legs,proto3"                              json:"legs,omitempty"`
	Description string    `protobuf:"bytes,5,opt,name=description,proto3"                       json:"description,omitempty"`
	MaxRetry    int32     `protobuf:"varint,6,opt,name=max_retry,json=maxRetry,proto3"          json:"max_retry,omitempty"`
}

func (x *CreateTransactionRequest) Reset()         { *x = CreateTransactionRequest{} }
func (x *CreateTransactionRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*CreateTransactionRequest) ProtoMessage()    {}

// CreateTransactionResponse.
type CreateTransactionResponse struct {
	Code         int32  `protobuf:"varint,1,opt,name=code,proto3"                              json:"code,omitempty"`
	Message      string `protobuf:"bytes,2,opt,name=message,proto3"                            json:"message,omitempty"`
	OrderNo      string `protobuf:"bytes,3,opt,name=order_no,json=orderNo,proto3"              json:"order_no,omitempty"`
	Status       int32  `protobuf:"varint,4,opt,name=status,proto3"                            json:"status,omitempty"` // 0..3
	VoucherNo    string `protobuf:"bytes,5,opt,name=voucher_no,json=voucherNo,proto3"          json:"voucher_no,omitempty"`
	ErrorMessage string `protobuf:"bytes,6,opt,name=error_message,json=errorMessage,proto3"    json:"error_message,omitempty"`
}

func (x *CreateTransactionResponse) Reset()         { *x = CreateTransactionResponse{} }
func (x *CreateTransactionResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*CreateTransactionResponse) ProtoMessage()    {}

// AccountTypeInfo 元数据查询返回项.
type AccountTypeInfo struct {
	AccountType      string `protobuf:"bytes,1,opt,name=account_type,json=accountType,proto3"             json:"account_type,omitempty"`
	AccountTypeName  string `protobuf:"bytes,2,opt,name=account_type_name,json=accountTypeName,proto3"    json:"account_type_name,omitempty"`
	OwnerType        int32  `protobuf:"varint,3,opt,name=owner_type,json=ownerType,proto3"                json:"owner_type,omitempty"`
	IsPlatform       int32  `protobuf:"varint,4,opt,name=is_platform,json=isPlatform,proto3"              json:"is_platform,omitempty"`
	BalanceDirection string `protobuf:"bytes,5,opt,name=balance_direction,json=balanceDirection,proto3"   json:"balance_direction,omitempty"`
	Description      string `protobuf:"bytes,6,opt,name=description,proto3"                                json:"description,omitempty"`
}

func (x *AccountTypeInfo) Reset()         { *x = AccountTypeInfo{} }
func (x *AccountTypeInfo) String() string { return fmt.Sprintf("%+v", *x) }
func (*AccountTypeInfo) ProtoMessage()    {}

// ListAccountTypesRequest.
type ListAccountTypesRequest struct{}

func (x *ListAccountTypesRequest) Reset()         { *x = ListAccountTypesRequest{} }
func (x *ListAccountTypesRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListAccountTypesRequest) ProtoMessage()    {}

// ListAccountTypesResponse.
type ListAccountTypesResponse struct {
	Code    int32              `protobuf:"varint,1,opt,name=code,proto3"   json:"code,omitempty"`
	Message string             `protobuf:"bytes,2,opt,name=message,proto3" json:"message,omitempty"`
	Items   []*AccountTypeInfo `protobuf:"bytes,3,rep,name=items,proto3"   json:"items,omitempty"`
}

func (x *ListAccountTypesResponse) Reset()         { *x = ListAccountTypesResponse{} }
func (x *ListAccountTypesResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListAccountTypesResponse) ProtoMessage()    {}

// TransactionRule.
type TransactionRule struct {
	Id              int64  `protobuf:"varint,1,opt,name=id,proto3"                                          json:"id,omitempty"`
	ProductCode     string `protobuf:"bytes,2,opt,name=product_code,json=productCode,proto3"               json:"product_code,omitempty"`
	EventCode       string `protobuf:"bytes,3,opt,name=event_code,json=eventCode,proto3"                   json:"event_code,omitempty"`
	DebitSubjectId  string `protobuf:"bytes,4,opt,name=debit_subject_id,json=debitSubjectId,proto3"        json:"debit_subject_id,omitempty"`
	CreditSubjectId string `protobuf:"bytes,5,opt,name=credit_subject_id,json=creditSubjectId,proto3"      json:"credit_subject_id,omitempty"`
	FromDirection   string `protobuf:"bytes,6,opt,name=from_direction,json=fromDirection,proto3"           json:"from_direction,omitempty"`
	ToDirection     string `protobuf:"bytes,7,opt,name=to_direction,json=toDirection,proto3"               json:"to_direction,omitempty"`
	Description     string `protobuf:"bytes,8,opt,name=description,proto3"                                  json:"description,omitempty"`
}

func (x *TransactionRule) Reset()         { *x = TransactionRule{} }
func (x *TransactionRule) String() string { return fmt.Sprintf("%+v", *x) }
func (*TransactionRule) ProtoMessage()    {}

// ListTransactionRulesRequest.
type ListTransactionRulesRequest struct {
	ProductCode string `protobuf:"bytes,1,opt,name=product_code,json=productCode,proto3" json:"product_code,omitempty"`
}

func (x *ListTransactionRulesRequest) Reset()         { *x = ListTransactionRulesRequest{} }
func (x *ListTransactionRulesRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListTransactionRulesRequest) ProtoMessage()    {}

// ListTransactionRulesResponse.
type ListTransactionRulesResponse struct {
	Code    int32              `protobuf:"varint,1,opt,name=code,proto3"   json:"code,omitempty"`
	Message string             `protobuf:"bytes,2,opt,name=message,proto3" json:"message,omitempty"`
	Items   []*TransactionRule `protobuf:"bytes,3,rep,name=items,proto3"   json:"items,omitempty"`
}

func (x *ListTransactionRulesResponse) Reset()         { *x = ListTransactionRulesResponse{} }
func (x *ListTransactionRulesResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListTransactionRulesResponse) ProtoMessage()    {}

// ─── Server interface ──────────────────────────────────────────────────────

// TransactionServiceServer.
type TransactionServiceServer interface {
	CreateTransaction(context.Context, *CreateTransactionRequest) (*CreateTransactionResponse, error)
	ListAccountTypes(context.Context, *ListAccountTypesRequest) (*ListAccountTypesResponse, error)
	ListTransactionRules(context.Context, *ListTransactionRulesRequest) (*ListTransactionRulesResponse, error)
	mustEmbedUnimplementedTransactionServiceServer()
}

// UnimplementedTransactionServiceServer 默认 not-implemented; 实现方 embed 它做前向兼容.
type UnimplementedTransactionServiceServer struct{}

func (UnimplementedTransactionServiceServer) CreateTransaction(context.Context, *CreateTransactionRequest) (*CreateTransactionResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method CreateTransaction not implemented")
}
func (UnimplementedTransactionServiceServer) ListAccountTypes(context.Context, *ListAccountTypesRequest) (*ListAccountTypesResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListAccountTypes not implemented")
}
func (UnimplementedTransactionServiceServer) ListTransactionRules(context.Context, *ListTransactionRulesRequest) (*ListTransactionRulesResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListTransactionRules not implemented")
}
func (UnimplementedTransactionServiceServer) mustEmbedUnimplementedTransactionServiceServer() {}

// ─── Client interface ──────────────────────────────────────────────────────

// TransactionServiceClient.
type TransactionServiceClient interface {
	CreateTransaction(ctx context.Context, in *CreateTransactionRequest, opts ...grpc.CallOption) (*CreateTransactionResponse, error)
	ListAccountTypes(ctx context.Context, in *ListAccountTypesRequest, opts ...grpc.CallOption) (*ListAccountTypesResponse, error)
	ListTransactionRules(ctx context.Context, in *ListTransactionRulesRequest, opts ...grpc.CallOption) (*ListTransactionRulesResponse, error)
}

type transactionServiceClient struct{ cc grpc.ClientConnInterface }

// NewTransactionServiceClient.
func NewTransactionServiceClient(cc grpc.ClientConnInterface) TransactionServiceClient {
	return &transactionServiceClient{cc}
}

func (c *transactionServiceClient) CreateTransaction(ctx context.Context, in *CreateTransactionRequest, opts ...grpc.CallOption) (*CreateTransactionResponse, error) {
	out := new(CreateTransactionResponse)
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	if err := c.cc.Invoke(ctx, "/accounting.v1.TransactionService/CreateTransaction", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}
func (c *transactionServiceClient) ListAccountTypes(ctx context.Context, in *ListAccountTypesRequest, opts ...grpc.CallOption) (*ListAccountTypesResponse, error) {
	out := new(ListAccountTypesResponse)
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	if err := c.cc.Invoke(ctx, "/accounting.v1.TransactionService/ListAccountTypes", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}
func (c *transactionServiceClient) ListTransactionRules(ctx context.Context, in *ListTransactionRulesRequest, opts ...grpc.CallOption) (*ListTransactionRulesResponse, error) {
	out := new(ListTransactionRulesResponse)
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	if err := c.cc.Invoke(ctx, "/accounting.v1.TransactionService/ListTransactionRules", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

// ─── Handlers + ServiceDesc ────────────────────────────────────────────────

func _TransactionService_CreateTransaction_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(CreateTransactionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(TransactionServiceServer).CreateTransaction(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.TransactionService/CreateTransaction"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(TransactionServiceServer).CreateTransaction(ctx, req.(*CreateTransactionRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _TransactionService_ListAccountTypes_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ListAccountTypesRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(TransactionServiceServer).ListAccountTypes(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.TransactionService/ListAccountTypes"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(TransactionServiceServer).ListAccountTypes(ctx, req.(*ListAccountTypesRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _TransactionService_ListTransactionRules_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ListTransactionRulesRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(TransactionServiceServer).ListTransactionRules(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.TransactionService/ListTransactionRules"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(TransactionServiceServer).ListTransactionRules(ctx, req.(*ListTransactionRulesRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// TransactionService_ServiceDesc.
var TransactionService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "accounting.v1.TransactionService",
	HandlerType: (*TransactionServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "CreateTransaction", Handler: _TransactionService_CreateTransaction_Handler},
		{MethodName: "ListAccountTypes", Handler: _TransactionService_ListAccountTypes_Handler},
		{MethodName: "ListTransactionRules", Handler: _TransactionService_ListTransactionRules_Handler},
	},
	Streams: []grpc.StreamDesc{},
}

// RegisterTransactionServiceServer.
func RegisterTransactionServiceServer(s grpc.ServiceRegistrar, srv TransactionServiceServer) {
	s.RegisterService(&TransactionService_ServiceDesc, srv)
}
