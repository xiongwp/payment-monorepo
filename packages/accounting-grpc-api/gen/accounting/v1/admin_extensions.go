// admin_extensions.go — hand-written extension types and the AccountingAdminService gRPC service.
// Uses legacy proto v1 struct-tag encoding compatible with grpc v1.60+.
package accountingv1

import (
	"context"
	"fmt"

	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// ─── AccountingAdminService — maintenance / batch-task RPCs ─────────────────

// ProcessAsyncTasksRequest triggers a batch run of pending async tasks.
type ProcessAsyncTasksRequest struct {
	// BatchSize is the max number of tasks to process per shard per call (default 100).
	BatchSize int32 `protobuf:"varint,1,opt,name=batch_size,json=batchSize,proto3" json:"batch_size,omitempty"`
}

func (x *ProcessAsyncTasksRequest) Reset()         { *x = ProcessAsyncTasksRequest{} }
func (x *ProcessAsyncTasksRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*ProcessAsyncTasksRequest) ProtoMessage()    {}

// ProcessAsyncTasksResponse reports how many tasks were processed.
type ProcessAsyncTasksResponse struct {
	Code      int32  `protobuf:"varint,1,opt,name=code,proto3"      json:"code,omitempty"`
	Message   string `protobuf:"bytes,2,opt,name=message,proto3"    json:"message,omitempty"`
	Processed int32  `protobuf:"varint,3,opt,name=processed,proto3" json:"processed,omitempty"`
}

func (x *ProcessAsyncTasksResponse) Reset()         { *x = ProcessAsyncTasksResponse{} }
func (x *ProcessAsyncTasksResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*ProcessAsyncTasksResponse) ProtoMessage()    {}

// DayCutWatchdogRequest triggers a scan for stuck day-cut shards.
type DayCutWatchdogRequest struct {
	// StuckThresholdSeconds is how many seconds a shard must be unchanged to be considered stuck (default 300).
	StuckThresholdSeconds int32 `protobuf:"varint,1,opt,name=stuck_threshold_seconds,json=stuckThresholdSeconds,proto3" json:"stuck_threshold_seconds,omitempty"`
}

func (x *DayCutWatchdogRequest) Reset()         { *x = DayCutWatchdogRequest{} }
func (x *DayCutWatchdogRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*DayCutWatchdogRequest) ProtoMessage()    {}

// DayCutWatchdogResponse reports the watchdog result.
type DayCutWatchdogResponse struct {
	Code    int32  `protobuf:"varint,1,opt,name=code,proto3"    json:"code,omitempty"`
	Message string `protobuf:"bytes,2,opt,name=message,proto3"  json:"message,omitempty"`
}

func (x *DayCutWatchdogResponse) Reset()         { *x = DayCutWatchdogResponse{} }
func (x *DayCutWatchdogResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*DayCutWatchdogResponse) ProtoMessage()    {}

// ManualTask describes a single task that needs human intervention.
type ManualTask struct {
	TaskId       string `protobuf:"bytes,1,opt,name=task_id,json=taskId,proto3"             json:"task_id,omitempty"`
	TaskType     string `protobuf:"bytes,2,opt,name=task_type,json=taskType,proto3"         json:"task_type,omitempty"`
	BusinessNo   string `protobuf:"bytes,3,opt,name=business_no,json=businessNo,proto3"     json:"business_no,omitempty"`
	ErrorMessage string `protobuf:"bytes,4,opt,name=error_message,json=errorMessage,proto3" json:"error_message,omitempty"`
	RetryCount   int32  `protobuf:"varint,5,opt,name=retry_count,json=retryCount,proto3"    json:"retry_count,omitempty"`
}

func (x *ManualTask) Reset()         { *x = ManualTask{} }
func (x *ManualTask) String() string { return fmt.Sprintf("%+v", *x) }
func (*ManualTask) ProtoMessage()    {}

// ListManualTasksRequest requests the list of tasks pending manual processing.
type ListManualTasksRequest struct {
	// Limit is the maximum number of tasks to return (default 50).
	Limit int32 `protobuf:"varint,1,opt,name=limit,proto3" json:"limit,omitempty"`
}

func (x *ListManualTasksRequest) Reset()         { *x = ListManualTasksRequest{} }
func (x *ListManualTasksRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListManualTasksRequest) ProtoMessage()    {}

// ListManualTasksResponse returns tasks awaiting manual processing.
type ListManualTasksResponse struct {
	Code    int32         `protobuf:"varint,1,opt,name=code,proto3"   json:"code,omitempty"`
	Message string        `protobuf:"bytes,2,opt,name=message,proto3" json:"message,omitempty"`
	Tasks   []*ManualTask `protobuf:"bytes,3,rep,name=tasks,proto3"   json:"tasks,omitempty"`
}

func (x *ListManualTasksResponse) Reset()         { *x = ListManualTasksResponse{} }
func (x *ListManualTasksResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListManualTasksResponse) ProtoMessage()    {}

// RecoverStuckTasksRequest triggers recovery of async tasks stuck in PROCESSING.
type RecoverStuckTasksRequest struct {
	// StuckThresholdSeconds is how long a task must be in PROCESSING before it is reset (default 300).
	StuckThresholdSeconds int32 `protobuf:"varint,1,opt,name=stuck_threshold_seconds,json=stuckThresholdSeconds,proto3" json:"stuck_threshold_seconds,omitempty"`
}

func (x *RecoverStuckTasksRequest) Reset()         { *x = RecoverStuckTasksRequest{} }
func (x *RecoverStuckTasksRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*RecoverStuckTasksRequest) ProtoMessage()    {}

// RecoverStuckTasksResponse reports how many tasks were recovered.
type RecoverStuckTasksResponse struct {
	Code      int32  `protobuf:"varint,1,opt,name=code,proto3"      json:"code,omitempty"`
	Message   string `protobuf:"bytes,2,opt,name=message,proto3"    json:"message,omitempty"`
	Recovered int64  `protobuf:"varint,3,opt,name=recovered,proto3" json:"recovered,omitempty"`
}

func (x *RecoverStuckTasksResponse) Reset()         { *x = RecoverStuckTasksResponse{} }
func (x *RecoverStuckTasksResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*RecoverStuckTasksResponse) ProtoMessage()    {}

// ─── Buffer account config reload ────────────────────────────────────────────

// ReloadBufferAccountConfigRequest triggers a reload of the buffer account config from account_meta DB.
type ReloadBufferAccountConfigRequest struct{}

func (x *ReloadBufferAccountConfigRequest) Reset()         { *x = ReloadBufferAccountConfigRequest{} }
func (x *ReloadBufferAccountConfigRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*ReloadBufferAccountConfigRequest) ProtoMessage()    {}

// ReloadBufferAccountConfigResponse reports the reload result.
type ReloadBufferAccountConfigResponse struct {
	Code         int32  `protobuf:"varint,1,opt,name=code,proto3"          json:"code,omitempty"`
	Message      string `protobuf:"bytes,2,opt,name=message,proto3"        json:"message,omitempty"`
	AccountCount int32  `protobuf:"varint,3,opt,name=account_count,json=accountCount,proto3" json:"account_count,omitempty"`
}

func (x *ReloadBufferAccountConfigResponse) Reset()         { *x = ReloadBufferAccountConfigResponse{} }
func (x *ReloadBufferAccountConfigResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*ReloadBufferAccountConfigResponse) ProtoMessage()    {}

// ─── Buffer account CRUD ─────────────────────────────────────────────────────

// BufferAccountEntry represents a single buffer account configuration record.
type BufferAccountEntry struct {
	Id                 int64  `protobuf:"varint,1,opt,name=id,proto3"                                                json:"id,omitempty"`
	AccountNo          string `protobuf:"bytes,2,opt,name=account_no,json=accountNo,proto3"                          json:"account_no,omitempty"`
	FlushIntervalLevel int32  `protobuf:"varint,3,opt,name=flush_interval_level,json=flushIntervalLevel,proto3"     json:"flush_interval_level,omitempty"`
	Enabled            bool   `protobuf:"varint,4,opt,name=enabled,proto3"                                           json:"enabled,omitempty"`
	Description        string `protobuf:"bytes,5,opt,name=description,proto3"                                        json:"description,omitempty"`
	CreatedAt          string `protobuf:"bytes,6,opt,name=created_at,json=createdAt,proto3"                         json:"created_at,omitempty"`
	UpdatedAt          string `protobuf:"bytes,7,opt,name=updated_at,json=updatedAt,proto3"                         json:"updated_at,omitempty"`
}

func (x *BufferAccountEntry) Reset()         { *x = BufferAccountEntry{} }
func (x *BufferAccountEntry) String() string { return fmt.Sprintf("%+v", *x) }
func (*BufferAccountEntry) ProtoMessage()    {}

// ListBufferAccountsRequest requests all buffer account configuration records.
type ListBufferAccountsRequest struct{}

func (x *ListBufferAccountsRequest) Reset()         { *x = ListBufferAccountsRequest{} }
func (x *ListBufferAccountsRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListBufferAccountsRequest) ProtoMessage()    {}

// ListBufferAccountsResponse returns the list of buffer account configurations.
type ListBufferAccountsResponse struct {
	Code    int32                 `protobuf:"varint,1,opt,name=code,proto3"   json:"code,omitempty"`
	Message string                `protobuf:"bytes,2,opt,name=message,proto3" json:"message,omitempty"`
	Items   []*BufferAccountEntry `protobuf:"bytes,3,rep,name=items,proto3"   json:"items,omitempty"`
}

func (x *ListBufferAccountsResponse) Reset()         { *x = ListBufferAccountsResponse{} }
func (x *ListBufferAccountsResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*ListBufferAccountsResponse) ProtoMessage()    {}

// CreateBufferAccountRequest creates a new buffer account configuration entry.
type CreateBufferAccountRequest struct {
	AccountNo          string `protobuf:"bytes,1,opt,name=account_no,json=accountNo,proto3"                      json:"account_no,omitempty"`
	FlushIntervalLevel int32  `protobuf:"varint,2,opt,name=flush_interval_level,json=flushIntervalLevel,proto3" json:"flush_interval_level,omitempty"`
	Description        string `protobuf:"bytes,3,opt,name=description,proto3"                                   json:"description,omitempty"`
}

func (x *CreateBufferAccountRequest) Reset()         { *x = CreateBufferAccountRequest{} }
func (x *CreateBufferAccountRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*CreateBufferAccountRequest) ProtoMessage()    {}

// CreateBufferAccountResponse returns the created entry.
type CreateBufferAccountResponse struct {
	Code    int32               `protobuf:"varint,1,opt,name=code,proto3"   json:"code,omitempty"`
	Message string              `protobuf:"bytes,2,opt,name=message,proto3" json:"message,omitempty"`
	Item    *BufferAccountEntry `protobuf:"bytes,3,opt,name=item,proto3"    json:"item,omitempty"`
}

func (x *CreateBufferAccountResponse) Reset()         { *x = CreateBufferAccountResponse{} }
func (x *CreateBufferAccountResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*CreateBufferAccountResponse) ProtoMessage()    {}

// UpdateBufferAccountRequest updates an existing buffer account configuration entry.
type UpdateBufferAccountRequest struct {
	Id                 int64  `protobuf:"varint,1,opt,name=id,proto3"                                                json:"id,omitempty"`
	Enabled            bool   `protobuf:"varint,2,opt,name=enabled,proto3"                                           json:"enabled,omitempty"`
	FlushIntervalLevel int32  `protobuf:"varint,3,opt,name=flush_interval_level,json=flushIntervalLevel,proto3"     json:"flush_interval_level,omitempty"`
	Description        string `protobuf:"bytes,4,opt,name=description,proto3"                                        json:"description,omitempty"`
}

func (x *UpdateBufferAccountRequest) Reset()         { *x = UpdateBufferAccountRequest{} }
func (x *UpdateBufferAccountRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*UpdateBufferAccountRequest) ProtoMessage()    {}

// UpdateBufferAccountResponse confirms the update.
type UpdateBufferAccountResponse struct {
	Code    int32  `protobuf:"varint,1,opt,name=code,proto3"   json:"code,omitempty"`
	Message string `protobuf:"bytes,2,opt,name=message,proto3" json:"message,omitempty"`
}

func (x *UpdateBufferAccountResponse) Reset()         { *x = UpdateBufferAccountResponse{} }
func (x *UpdateBufferAccountResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*UpdateBufferAccountResponse) ProtoMessage()    {}

// DeleteBufferAccountRequest deletes a buffer account configuration entry.
type DeleteBufferAccountRequest struct {
	Id int64 `protobuf:"varint,1,opt,name=id,proto3" json:"id,omitempty"`
}

func (x *DeleteBufferAccountRequest) Reset()         { *x = DeleteBufferAccountRequest{} }
func (x *DeleteBufferAccountRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*DeleteBufferAccountRequest) ProtoMessage()    {}

// DeleteBufferAccountResponse confirms the deletion.
type DeleteBufferAccountResponse struct {
	Code    int32  `protobuf:"varint,1,opt,name=code,proto3"   json:"code,omitempty"`
	Message string `protobuf:"bytes,2,opt,name=message,proto3" json:"message,omitempty"`
}

func (x *DeleteBufferAccountResponse) Reset()         { *x = DeleteBufferAccountResponse{} }
func (x *DeleteBufferAccountResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*DeleteBufferAccountResponse) ProtoMessage()    {}

// NOTE: ListAccountsByUserAndBusinessTypeRequest / Response and
// RebuildHotAccountsRequest / Response / RebuildHotAccountEntry used to live
// here as hand-written types. They are now declared in proto/accounting.proto
// and generated into accounting.pb.go by `make gen-go`, so the definitions
// below were removed to avoid duplicate-symbol errors after regeneration.
// If you need to re-introduce a hand-written type for a new RPC, mirror the
// BufferAccount* pattern above and keep its proto declaration out of
// accounting.proto.

// ─── AccountingAdminServiceClient ───────────────────────────────────────────

// AccountingAdminServiceClient is the client API for AccountingAdminService.
type AccountingAdminServiceClient interface {
	ProcessAsyncTasks(ctx context.Context, in *ProcessAsyncTasksRequest, opts ...grpc.CallOption) (*ProcessAsyncTasksResponse, error)
	DayCutWatchdog(ctx context.Context, in *DayCutWatchdogRequest, opts ...grpc.CallOption) (*DayCutWatchdogResponse, error)
	ListManualTasks(ctx context.Context, in *ListManualTasksRequest, opts ...grpc.CallOption) (*ListManualTasksResponse, error)
	RecoverStuckTasks(ctx context.Context, in *RecoverStuckTasksRequest, opts ...grpc.CallOption) (*RecoverStuckTasksResponse, error)
	ReloadBufferAccountConfig(ctx context.Context, in *ReloadBufferAccountConfigRequest, opts ...grpc.CallOption) (*ReloadBufferAccountConfigResponse, error)
	ListBufferAccounts(ctx context.Context, in *ListBufferAccountsRequest, opts ...grpc.CallOption) (*ListBufferAccountsResponse, error)
	CreateBufferAccount(ctx context.Context, in *CreateBufferAccountRequest, opts ...grpc.CallOption) (*CreateBufferAccountResponse, error)
	UpdateBufferAccount(ctx context.Context, in *UpdateBufferAccountRequest, opts ...grpc.CallOption) (*UpdateBufferAccountResponse, error)
	DeleteBufferAccount(ctx context.Context, in *DeleteBufferAccountRequest, opts ...grpc.CallOption) (*DeleteBufferAccountResponse, error)
}

type accountingAdminServiceClient struct {
	cc grpc.ClientConnInterface
}

// NewAccountingAdminServiceClient creates a new AccountingAdminServiceClient.
func NewAccountingAdminServiceClient(cc grpc.ClientConnInterface) AccountingAdminServiceClient {
	return &accountingAdminServiceClient{cc}
}

func (c *accountingAdminServiceClient) ProcessAsyncTasks(ctx context.Context, in *ProcessAsyncTasksRequest, opts ...grpc.CallOption) (*ProcessAsyncTasksResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(ProcessAsyncTasksResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.AccountingAdminService/ProcessAsyncTasks", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *accountingAdminServiceClient) DayCutWatchdog(ctx context.Context, in *DayCutWatchdogRequest, opts ...grpc.CallOption) (*DayCutWatchdogResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(DayCutWatchdogResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.AccountingAdminService/DayCutWatchdog", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *accountingAdminServiceClient) ListManualTasks(ctx context.Context, in *ListManualTasksRequest, opts ...grpc.CallOption) (*ListManualTasksResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(ListManualTasksResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.AccountingAdminService/ListManualTasks", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *accountingAdminServiceClient) RecoverStuckTasks(ctx context.Context, in *RecoverStuckTasksRequest, opts ...grpc.CallOption) (*RecoverStuckTasksResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(RecoverStuckTasksResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.AccountingAdminService/RecoverStuckTasks", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *accountingAdminServiceClient) ReloadBufferAccountConfig(ctx context.Context, in *ReloadBufferAccountConfigRequest, opts ...grpc.CallOption) (*ReloadBufferAccountConfigResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(ReloadBufferAccountConfigResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.AccountingAdminService/ReloadBufferAccountConfig", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *accountingAdminServiceClient) ListBufferAccounts(ctx context.Context, in *ListBufferAccountsRequest, opts ...grpc.CallOption) (*ListBufferAccountsResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(ListBufferAccountsResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.AccountingAdminService/ListBufferAccounts", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *accountingAdminServiceClient) CreateBufferAccount(ctx context.Context, in *CreateBufferAccountRequest, opts ...grpc.CallOption) (*CreateBufferAccountResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(CreateBufferAccountResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.AccountingAdminService/CreateBufferAccount", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *accountingAdminServiceClient) UpdateBufferAccount(ctx context.Context, in *UpdateBufferAccountRequest, opts ...grpc.CallOption) (*UpdateBufferAccountResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(UpdateBufferAccountResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.AccountingAdminService/UpdateBufferAccount", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *accountingAdminServiceClient) DeleteBufferAccount(ctx context.Context, in *DeleteBufferAccountRequest, opts ...grpc.CallOption) (*DeleteBufferAccountResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(DeleteBufferAccountResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.AccountingAdminService/DeleteBufferAccount", in, out, cOpts...); err != nil {
		return nil, err
	}
	return out, nil
}

// ─── AccountingAdminServiceServer ───────────────────────────────────────────

// AccountingAdminServiceServer is the server API for AccountingAdminService.
type AccountingAdminServiceServer interface {
	ProcessAsyncTasks(context.Context, *ProcessAsyncTasksRequest) (*ProcessAsyncTasksResponse, error)
	DayCutWatchdog(context.Context, *DayCutWatchdogRequest) (*DayCutWatchdogResponse, error)
	ListManualTasks(context.Context, *ListManualTasksRequest) (*ListManualTasksResponse, error)
	RecoverStuckTasks(context.Context, *RecoverStuckTasksRequest) (*RecoverStuckTasksResponse, error)
	ReloadBufferAccountConfig(context.Context, *ReloadBufferAccountConfigRequest) (*ReloadBufferAccountConfigResponse, error)
	ListBufferAccounts(context.Context, *ListBufferAccountsRequest) (*ListBufferAccountsResponse, error)
	CreateBufferAccount(context.Context, *CreateBufferAccountRequest) (*CreateBufferAccountResponse, error)
	UpdateBufferAccount(context.Context, *UpdateBufferAccountRequest) (*UpdateBufferAccountResponse, error)
	DeleteBufferAccount(context.Context, *DeleteBufferAccountRequest) (*DeleteBufferAccountResponse, error)
	mustEmbedUnimplementedAccountingAdminServiceServer()
}

// UnimplementedAccountingAdminServiceServer provides default implementations.
type UnimplementedAccountingAdminServiceServer struct{}

func (UnimplementedAccountingAdminServiceServer) ProcessAsyncTasks(context.Context, *ProcessAsyncTasksRequest) (*ProcessAsyncTasksResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ProcessAsyncTasks not implemented")
}
func (UnimplementedAccountingAdminServiceServer) DayCutWatchdog(context.Context, *DayCutWatchdogRequest) (*DayCutWatchdogResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method DayCutWatchdog not implemented")
}
func (UnimplementedAccountingAdminServiceServer) ListManualTasks(context.Context, *ListManualTasksRequest) (*ListManualTasksResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListManualTasks not implemented")
}
func (UnimplementedAccountingAdminServiceServer) RecoverStuckTasks(context.Context, *RecoverStuckTasksRequest) (*RecoverStuckTasksResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method RecoverStuckTasks not implemented")
}
func (UnimplementedAccountingAdminServiceServer) ReloadBufferAccountConfig(context.Context, *ReloadBufferAccountConfigRequest) (*ReloadBufferAccountConfigResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ReloadBufferAccountConfig not implemented")
}
func (UnimplementedAccountingAdminServiceServer) ListBufferAccounts(context.Context, *ListBufferAccountsRequest) (*ListBufferAccountsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListBufferAccounts not implemented")
}
func (UnimplementedAccountingAdminServiceServer) CreateBufferAccount(context.Context, *CreateBufferAccountRequest) (*CreateBufferAccountResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method CreateBufferAccount not implemented")
}
func (UnimplementedAccountingAdminServiceServer) UpdateBufferAccount(context.Context, *UpdateBufferAccountRequest) (*UpdateBufferAccountResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method UpdateBufferAccount not implemented")
}
func (UnimplementedAccountingAdminServiceServer) DeleteBufferAccount(context.Context, *DeleteBufferAccountRequest) (*DeleteBufferAccountResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method DeleteBufferAccount not implemented")
}
func (UnimplementedAccountingAdminServiceServer) mustEmbedUnimplementedAccountingAdminServiceServer() {
}

// UnsafeAccountingAdminServiceServer may be embedded to opt out of forward compatibility.
type UnsafeAccountingAdminServiceServer interface {
	mustEmbedUnimplementedAccountingAdminServiceServer()
}

// ─── gRPC handler functions ──────────────────────────────────────────────────

func _AccountingAdminService_ProcessAsyncTasks_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ProcessAsyncTasksRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AccountingAdminServiceServer).ProcessAsyncTasks(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/accounting.v1.AccountingAdminService/ProcessAsyncTasks",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AccountingAdminServiceServer).ProcessAsyncTasks(ctx, req.(*ProcessAsyncTasksRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _AccountingAdminService_DayCutWatchdog_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(DayCutWatchdogRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AccountingAdminServiceServer).DayCutWatchdog(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/accounting.v1.AccountingAdminService/DayCutWatchdog",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AccountingAdminServiceServer).DayCutWatchdog(ctx, req.(*DayCutWatchdogRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _AccountingAdminService_ListManualTasks_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ListManualTasksRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AccountingAdminServiceServer).ListManualTasks(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/accounting.v1.AccountingAdminService/ListManualTasks",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AccountingAdminServiceServer).ListManualTasks(ctx, req.(*ListManualTasksRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _AccountingAdminService_RecoverStuckTasks_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(RecoverStuckTasksRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AccountingAdminServiceServer).RecoverStuckTasks(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/accounting.v1.AccountingAdminService/RecoverStuckTasks",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AccountingAdminServiceServer).RecoverStuckTasks(ctx, req.(*RecoverStuckTasksRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _AccountingAdminService_ReloadBufferAccountConfig_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ReloadBufferAccountConfigRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AccountingAdminServiceServer).ReloadBufferAccountConfig(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.AccountingAdminService/ReloadBufferAccountConfig"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AccountingAdminServiceServer).ReloadBufferAccountConfig(ctx, req.(*ReloadBufferAccountConfigRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _AccountingAdminService_ListBufferAccounts_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ListBufferAccountsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AccountingAdminServiceServer).ListBufferAccounts(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.AccountingAdminService/ListBufferAccounts"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AccountingAdminServiceServer).ListBufferAccounts(ctx, req.(*ListBufferAccountsRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _AccountingAdminService_CreateBufferAccount_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(CreateBufferAccountRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AccountingAdminServiceServer).CreateBufferAccount(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.AccountingAdminService/CreateBufferAccount"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AccountingAdminServiceServer).CreateBufferAccount(ctx, req.(*CreateBufferAccountRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _AccountingAdminService_UpdateBufferAccount_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(UpdateBufferAccountRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AccountingAdminServiceServer).UpdateBufferAccount(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.AccountingAdminService/UpdateBufferAccount"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AccountingAdminServiceServer).UpdateBufferAccount(ctx, req.(*UpdateBufferAccountRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _AccountingAdminService_DeleteBufferAccount_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(DeleteBufferAccountRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(AccountingAdminServiceServer).DeleteBufferAccount(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/accounting.v1.AccountingAdminService/DeleteBufferAccount"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(AccountingAdminServiceServer).DeleteBufferAccount(ctx, req.(*DeleteBufferAccountRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// AccountingAdminService_ServiceDesc is the grpc.ServiceDesc for AccountingAdminService.
var AccountingAdminService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "accounting.v1.AccountingAdminService",
	HandlerType: (*AccountingAdminServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "ProcessAsyncTasks", Handler: _AccountingAdminService_ProcessAsyncTasks_Handler},
		{MethodName: "DayCutWatchdog", Handler: _AccountingAdminService_DayCutWatchdog_Handler},
		{MethodName: "ListManualTasks", Handler: _AccountingAdminService_ListManualTasks_Handler},
		{MethodName: "RecoverStuckTasks", Handler: _AccountingAdminService_RecoverStuckTasks_Handler},
		{MethodName: "ReloadBufferAccountConfig", Handler: _AccountingAdminService_ReloadBufferAccountConfig_Handler},
		{MethodName: "ListBufferAccounts", Handler: _AccountingAdminService_ListBufferAccounts_Handler},
		{MethodName: "CreateBufferAccount", Handler: _AccountingAdminService_CreateBufferAccount_Handler},
		{MethodName: "UpdateBufferAccount", Handler: _AccountingAdminService_UpdateBufferAccount_Handler},
		{MethodName: "DeleteBufferAccount", Handler: _AccountingAdminService_DeleteBufferAccount_Handler},
	},
	Streams: []grpc.StreamDesc{},
}

// RegisterAccountingAdminServiceServer registers the AccountingAdminService implementation with the gRPC server.
func RegisterAccountingAdminServiceServer(s grpc.ServiceRegistrar, srv AccountingAdminServiceServer) {
	s.RegisterService(&AccountingAdminService_ServiceDesc, srv)
}
