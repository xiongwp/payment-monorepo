// freeze_extensions.go — hand-written FreezeService gRPC extension.
// Supports the freeze / unfreeze-and-debit / unfreeze-and-return flow for
// payment and withdrawal scenarios where funds must be held before the
// actual payment channel call completes.
package accountingv1

import (
	"context"
	"fmt"

	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// ─── Request / Response types ────────────────────────────────────────────────

// FreezeBalanceRequest freezes a portion of an account's available_balance.
// The balance visible to the user does not change; only available_balance
// decreases and frozen_balance increases by the same amount.
//
// OrderNo is the caller-supplied idempotency key (globally unique per freeze).
// BusinessNo / BusinessType are stored in the freeze order for audit purposes.
type FreezeBalanceRequest struct {
	OrderNo      string       `protobuf:"bytes,1,opt,name=order_no,json=orderNo,proto3"                            json:"order_no,omitempty"`
	AccountNo    string       `protobuf:"bytes,2,opt,name=account_no,json=accountNo,proto3"                        json:"account_no,omitempty"`
	BusinessNo   string       `protobuf:"bytes,3,opt,name=business_no,json=businessNo,proto3"                      json:"business_no,omitempty"`
	BusinessType BusinessType `protobuf:"varint,4,opt,name=business_type,json=businessType,proto3,enum=accounting.v1.BusinessType" json:"business_type,omitempty"`
	// Amount is in the smallest currency unit × 100 (same convention as the rest of the system).
	Amount      int64  `protobuf:"varint,5,opt,name=amount,proto3"   json:"amount,omitempty"`
	Currency    string `protobuf:"bytes,6,opt,name=currency,proto3"  json:"currency,omitempty"`
	Description string `protobuf:"bytes,7,opt,name=description,proto3" json:"description,omitempty"`
}

func (x *FreezeBalanceRequest) Reset()         { *x = FreezeBalanceRequest{} }
func (x *FreezeBalanceRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*FreezeBalanceRequest) ProtoMessage()    {}

// FreezeBalanceResponse is returned by FreezeBalance.
type FreezeBalanceResponse struct {
	Code    int32  `protobuf:"varint,1,opt,name=code,proto3"    json:"code,omitempty"`
	Message string `protobuf:"bytes,2,opt,name=message,proto3"  json:"message,omitempty"`
	// OrderNo echoes the caller-supplied idempotency key for convenience.
	OrderNo string `protobuf:"bytes,3,opt,name=order_no,json=orderNo,proto3" json:"order_no,omitempty"`
}

func (x *FreezeBalanceResponse) Reset()         { *x = FreezeBalanceResponse{} }
func (x *FreezeBalanceResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*FreezeBalanceResponse) ProtoMessage()    {}

// UnfreezeEntry is one leg of the double-entry bookkeeping executed during
// UnfreezeAndDebit.  Exactly one entry must have DebitAmount == frozen amount
// and AccountNo == the originally frozen account.
type UnfreezeEntry struct {
	AccountNo    string `protobuf:"bytes,1,opt,name=account_no,json=accountNo,proto3"      json:"account_no,omitempty"`
	DebitAmount  int64  `protobuf:"varint,2,opt,name=debit_amount,json=debitAmount,proto3"  json:"debit_amount,omitempty"`
	CreditAmount int64  `protobuf:"varint,3,opt,name=credit_amount,json=creditAmount,proto3" json:"credit_amount,omitempty"`
	Description  string `protobuf:"bytes,4,opt,name=description,proto3"                      json:"description,omitempty"`
}

func (x *UnfreezeEntry) Reset()         { *x = UnfreezeEntry{} }
func (x *UnfreezeEntry) String() string { return fmt.Sprintf("%+v", *x) }
func (*UnfreezeEntry) ProtoMessage()    {}

// UnfreezeAndDebitRequest executes the actual payment after a successful freeze:
//   - The frozen account is debited (balance -= amount, frozen_balance -= amount).
//   - All other entries are normal credits.
//
// The freeze order transitions FROZEN → SUCCESS.
// FreezeAccountNo is required for shard routing; it must match the account_no
// provided in the original FreezeBalance call.
type UnfreezeAndDebitRequest struct {
	FreezeOrderNo      string           `protobuf:"bytes,1,opt,name=freeze_order_no,json=freezeOrderNo,proto3"                                         json:"freeze_order_no,omitempty"`
	FreezeAccountNo    string           `protobuf:"bytes,2,opt,name=freeze_account_no,json=freezeAccountNo,proto3"                                     json:"freeze_account_no,omitempty"`
	FreezeBusinessNo   string           `protobuf:"bytes,3,opt,name=freeze_business_no,json=freezeBusinessNo,proto3"                                   json:"freeze_business_no,omitempty"`
	FreezeBusinessType BusinessType     `protobuf:"varint,4,opt,name=freeze_business_type,json=freezeBusinessType,proto3,enum=accounting.v1.BusinessType" json:"freeze_business_type,omitempty"`
	Entries            []*UnfreezeEntry `protobuf:"bytes,5,rep,name=entries,proto3"                                                                    json:"entries,omitempty"`
	Currency           string           `protobuf:"bytes,6,opt,name=currency,proto3"                                                                   json:"currency,omitempty"`
	Description        string           `protobuf:"bytes,7,opt,name=description,proto3"                                                                json:"description,omitempty"`
}

func (x *UnfreezeAndDebitRequest) Reset()         { *x = UnfreezeAndDebitRequest{} }
func (x *UnfreezeAndDebitRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*UnfreezeAndDebitRequest) ProtoMessage()    {}

// UnfreezeAndDebitResponse is returned by UnfreezeAndDebit.
type UnfreezeAndDebitResponse struct {
	Code           int32    `protobuf:"varint,1,opt,name=code,proto3"                          json:"code,omitempty"`
	Message        string   `protobuf:"bytes,2,opt,name=message,proto3"                        json:"message,omitempty"`
	VoucherNo      string   `protobuf:"bytes,3,opt,name=voucher_no,json=voucherNo,proto3"      json:"voucher_no,omitempty"`
	TransactionIds []string `protobuf:"bytes,4,rep,name=transaction_ids,json=transactionIds,proto3" json:"transaction_ids,omitempty"`
}

func (x *UnfreezeAndDebitResponse) Reset()         { *x = UnfreezeAndDebitResponse{} }
func (x *UnfreezeAndDebitResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*UnfreezeAndDebitResponse) ProtoMessage()    {}

// UnfreezeAndReturnRequest releases frozen funds back to available_balance
// when the payment attempt fails.  The freeze order transitions FROZEN → FAILED.
type UnfreezeAndReturnRequest struct {
	FreezeOrderNo      string       `protobuf:"bytes,1,opt,name=freeze_order_no,json=freezeOrderNo,proto3"                                         json:"freeze_order_no,omitempty"`
	FreezeAccountNo    string       `protobuf:"bytes,2,opt,name=freeze_account_no,json=freezeAccountNo,proto3"                                     json:"freeze_account_no,omitempty"`
	FreezeBusinessNo   string       `protobuf:"bytes,3,opt,name=freeze_business_no,json=freezeBusinessNo,proto3"                                   json:"freeze_business_no,omitempty"`
	FreezeBusinessType BusinessType `protobuf:"varint,4,opt,name=freeze_business_type,json=freezeBusinessType,proto3,enum=accounting.v1.BusinessType" json:"freeze_business_type,omitempty"`
}

func (x *UnfreezeAndReturnRequest) Reset()         { *x = UnfreezeAndReturnRequest{} }
func (x *UnfreezeAndReturnRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*UnfreezeAndReturnRequest) ProtoMessage()    {}

// UnfreezeAndReturnResponse is returned by UnfreezeAndReturn.
type UnfreezeAndReturnResponse struct {
	Code    int32  `protobuf:"varint,1,opt,name=code,proto3"   json:"code,omitempty"`
	Message string `protobuf:"bytes,2,opt,name=message,proto3" json:"message,omitempty"`
}

func (x *UnfreezeAndReturnResponse) Reset()         { *x = UnfreezeAndReturnResponse{} }
func (x *UnfreezeAndReturnResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*UnfreezeAndReturnResponse) ProtoMessage()    {}

// ─── FreezeServiceClient ─────────────────────────────────────────────────────

// FreezeServiceClient is the client API for FreezeService.
type FreezeServiceClient interface {
	FreezeBalance(ctx context.Context, in *FreezeBalanceRequest, opts ...grpc.CallOption) (*FreezeBalanceResponse, error)
	UnfreezeAndDebit(ctx context.Context, in *UnfreezeAndDebitRequest, opts ...grpc.CallOption) (*UnfreezeAndDebitResponse, error)
	UnfreezeAndReturn(ctx context.Context, in *UnfreezeAndReturnRequest, opts ...grpc.CallOption) (*UnfreezeAndReturnResponse, error)
}

type freezeServiceClient struct {
	cc grpc.ClientConnInterface
}

// NewFreezeServiceClient creates a new FreezeServiceClient.
func NewFreezeServiceClient(cc grpc.ClientConnInterface) FreezeServiceClient {
	return &freezeServiceClient{cc}
}

func (c *freezeServiceClient) FreezeBalance(ctx context.Context, in *FreezeBalanceRequest, opts ...grpc.CallOption) (*FreezeBalanceResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(FreezeBalanceResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.FreezeService/FreezeBalance", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *freezeServiceClient) UnfreezeAndDebit(ctx context.Context, in *UnfreezeAndDebitRequest, opts ...grpc.CallOption) (*UnfreezeAndDebitResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(UnfreezeAndDebitResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.FreezeService/UnfreezeAndDebit", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *freezeServiceClient) UnfreezeAndReturn(ctx context.Context, in *UnfreezeAndReturnRequest, opts ...grpc.CallOption) (*UnfreezeAndReturnResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(UnfreezeAndReturnResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.FreezeService/UnfreezeAndReturn", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

// ─── FreezeServiceServer ─────────────────────────────────────────────────────

// FreezeServiceServer is the server API for FreezeService.
type FreezeServiceServer interface {
	FreezeBalance(context.Context, *FreezeBalanceRequest) (*FreezeBalanceResponse, error)
	UnfreezeAndDebit(context.Context, *UnfreezeAndDebitRequest) (*UnfreezeAndDebitResponse, error)
	UnfreezeAndReturn(context.Context, *UnfreezeAndReturnRequest) (*UnfreezeAndReturnResponse, error)
	mustEmbedUnimplementedFreezeServiceServer()
}

// UnimplementedFreezeServiceServer provides default implementations.
type UnimplementedFreezeServiceServer struct{}

func (UnimplementedFreezeServiceServer) FreezeBalance(context.Context, *FreezeBalanceRequest) (*FreezeBalanceResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method FreezeBalance not implemented")
}
func (UnimplementedFreezeServiceServer) UnfreezeAndDebit(context.Context, *UnfreezeAndDebitRequest) (*UnfreezeAndDebitResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method UnfreezeAndDebit not implemented")
}
func (UnimplementedFreezeServiceServer) UnfreezeAndReturn(context.Context, *UnfreezeAndReturnRequest) (*UnfreezeAndReturnResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method UnfreezeAndReturn not implemented")
}
func (UnimplementedFreezeServiceServer) mustEmbedUnimplementedFreezeServiceServer() {}

// UnsafeFreezeServiceServer may be embedded to opt out of forward compatibility.
type UnsafeFreezeServiceServer interface {
	mustEmbedUnimplementedFreezeServiceServer()
}

// ─── gRPC handler functions ──────────────────────────────────────────────────

func _FreezeService_FreezeBalance_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(FreezeBalanceRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(FreezeServiceServer).FreezeBalance(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.FreezeService/FreezeBalance"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(FreezeServiceServer).FreezeBalance(ctx, req.(*FreezeBalanceRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _FreezeService_UnfreezeAndDebit_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(UnfreezeAndDebitRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(FreezeServiceServer).UnfreezeAndDebit(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.FreezeService/UnfreezeAndDebit"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(FreezeServiceServer).UnfreezeAndDebit(ctx, req.(*UnfreezeAndDebitRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _FreezeService_UnfreezeAndReturn_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(UnfreezeAndReturnRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(FreezeServiceServer).UnfreezeAndReturn(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.FreezeService/UnfreezeAndReturn"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(FreezeServiceServer).UnfreezeAndReturn(ctx, req.(*UnfreezeAndReturnRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// FreezeService_ServiceDesc is the grpc.ServiceDesc for FreezeService.
var FreezeService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "accounting.v1.FreezeService",
	HandlerType: (*FreezeServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "FreezeBalance", Handler: _FreezeService_FreezeBalance_Handler},
		{MethodName: "UnfreezeAndDebit", Handler: _FreezeService_UnfreezeAndDebit_Handler},
		{MethodName: "UnfreezeAndReturn", Handler: _FreezeService_UnfreezeAndReturn_Handler},
	},
	Streams: []grpc.StreamDesc{},
}

// RegisterFreezeServiceServer registers the FreezeService implementation with the gRPC server.
func RegisterFreezeServiceServer(s grpc.ServiceRegistrar, srv FreezeServiceServer) {
	s.RegisterService(&FreezeService_ServiceDesc, srv)
}
