// admin_extensions.go — hand-written proto message types for AccountingAdmin RPCs.
//
// gRPC Client/Server interfaces have been removed (Kitex switch). Only the
// request/response DTOs remain for use by server.go handlers.
package accountingv1

import "fmt"

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


// gRPC Client + Server interfaces / RegisterXxxServer / UnimplementedXxxServer
// 已删 (~330 行) — Kitex 切换后不再 generate 这些; AccountingAdmin RPCs 应
// 切到 Kitex idl, 通过 kitex_gen/.../accountingadminservice 暴露.
//
// 此文件目前只保留手写的 proto message 数据类型 (ProcessAsyncTasksRequest /
// BufferAccount / etc.) — accounting-system server.go 用作 RPC 请求/响应 DTO.
