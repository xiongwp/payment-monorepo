// accounting_grpc.go — SP-AC-7 gRPC client → accounting-system TransactionService.
//
// 取代旧的 HTTP /admin/transactions client (accounting_meta.go).
// HTTP 只留 ops / 系统管理 (cache reload, instance discovery 等),
// 业务路径 (CreateTransaction + 元数据查询) 全走 gRPC.
//
// Wire 协议:
//   ServiceName: accounting.v1.TransactionService
//   方法:
//     CreateTransaction(CreateTransactionRequest)    → CreateTransactionResponse
//     ListAccountTypes(ListAccountTypesRequest)      → ListAccountTypesResponse
//     ListTransactionRules(ListTransactionRulesRequest) → ListTransactionRulesResponse
//
// 跟 accounting-system/internal/grpc/transaction_service.go 互通靠:
//   - 同 ServiceName + 方法路径
//   - 同 protobuf 字段 tag 编码 (proto3 wire format)
// 不依赖 vendored pb 代码 — 故意 hand-written 以避免 internal/ 包跨模块引用问题.

package clients

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"

	"google.golang.org/grpc"
)

// ─── wire types (跟 server 端形态一致) ─────────────────────────────────────

type acctTxnLeg struct {
	EdgeFromNode  string `protobuf:"bytes,1,opt,name=edge_from_node,json=edgeFromNode,proto3"`
	EdgeToNode    string `protobuf:"bytes,2,opt,name=edge_to_node,json=edgeToNode,proto3"`
	FromAccountId string `protobuf:"bytes,3,opt,name=from_account_id,json=fromAccountId,proto3"`
	ToAccountId   string `protobuf:"bytes,4,opt,name=to_account_id,json=toAccountId,proto3"`
	Amount        string `protobuf:"bytes,5,opt,name=amount,proto3"`
	Currency      string `protobuf:"bytes,6,opt,name=currency,proto3"`
}

func (x *acctTxnLeg) Reset()         { *x = acctTxnLeg{} }
func (x *acctTxnLeg) String() string { return fmt.Sprintf("%+v", *x) }
func (*acctTxnLeg) ProtoMessage()    {}

type acctCreateTransactionRequest struct {
	OrderNo     string        `protobuf:"bytes,1,opt,name=order_no,json=orderNo,proto3"`
	ProductCode string        `protobuf:"bytes,2,opt,name=product_code,json=productCode,proto3"`
	EventCode   string        `protobuf:"bytes,3,opt,name=event_code,json=eventCode,proto3"`
	Legs        []*acctTxnLeg `protobuf:"bytes,4,rep,name=legs,proto3"`
	Description string        `protobuf:"bytes,5,opt,name=description,proto3"`
	MaxRetry    int32         `protobuf:"varint,6,opt,name=max_retry,json=maxRetry,proto3"`
}

func (x *acctCreateTransactionRequest) Reset()         { *x = acctCreateTransactionRequest{} }
func (x *acctCreateTransactionRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*acctCreateTransactionRequest) ProtoMessage()    {}

type acctCreateTransactionResponse struct {
	Code         int32  `protobuf:"varint,1,opt,name=code,proto3"`
	Message      string `protobuf:"bytes,2,opt,name=message,proto3"`
	OrderNo      string `protobuf:"bytes,3,opt,name=order_no,json=orderNo,proto3"`
	Status       int32  `protobuf:"varint,4,opt,name=status,proto3"`
	VoucherNo    string `protobuf:"bytes,5,opt,name=voucher_no,json=voucherNo,proto3"`
	ErrorMessage string `protobuf:"bytes,6,opt,name=error_message,json=errorMessage,proto3"`
}

func (x *acctCreateTransactionResponse) Reset()         { *x = acctCreateTransactionResponse{} }
func (x *acctCreateTransactionResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*acctCreateTransactionResponse) ProtoMessage()    {}

type acctAccountTypeInfo struct {
	AccountType      string `protobuf:"bytes,1,opt,name=account_type,json=accountType,proto3"`
	AccountTypeName  string `protobuf:"bytes,2,opt,name=account_type_name,json=accountTypeName,proto3"`
	OwnerType        int32  `protobuf:"varint,3,opt,name=owner_type,json=ownerType,proto3"`
	IsPlatform       int32  `protobuf:"varint,4,opt,name=is_platform,json=isPlatform,proto3"`
	BalanceDirection string `protobuf:"bytes,5,opt,name=balance_direction,json=balanceDirection,proto3"`
	Description      string `protobuf:"bytes,6,opt,name=description,proto3"`
}

func (x *acctAccountTypeInfo) Reset()         { *x = acctAccountTypeInfo{} }
func (x *acctAccountTypeInfo) String() string { return fmt.Sprintf("%+v", *x) }
func (*acctAccountTypeInfo) ProtoMessage()    {}

type acctListAccountTypesRequest struct{}

func (x *acctListAccountTypesRequest) Reset()         { *x = acctListAccountTypesRequest{} }
func (x *acctListAccountTypesRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*acctListAccountTypesRequest) ProtoMessage()    {}

type acctListAccountTypesResponse struct {
	Code    int32                  `protobuf:"varint,1,opt,name=code,proto3"`
	Message string                 `protobuf:"bytes,2,opt,name=message,proto3"`
	Items   []*acctAccountTypeInfo `protobuf:"bytes,3,rep,name=items,proto3"`
}

func (x *acctListAccountTypesResponse) Reset()         { *x = acctListAccountTypesResponse{} }
func (x *acctListAccountTypesResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*acctListAccountTypesResponse) ProtoMessage()    {}

type acctTransactionRule struct {
	Id              int64  `protobuf:"varint,1,opt,name=id,proto3"`
	ProductCode     string `protobuf:"bytes,2,opt,name=product_code,json=productCode,proto3"`
	EventCode       string `protobuf:"bytes,3,opt,name=event_code,json=eventCode,proto3"`
	DebitSubjectId  string `protobuf:"bytes,4,opt,name=debit_subject_id,json=debitSubjectId,proto3"`
	CreditSubjectId string `protobuf:"bytes,5,opt,name=credit_subject_id,json=creditSubjectId,proto3"`
	FromDirection   string `protobuf:"bytes,6,opt,name=from_direction,json=fromDirection,proto3"`
	ToDirection     string `protobuf:"bytes,7,opt,name=to_direction,json=toDirection,proto3"`
	Description     string `protobuf:"bytes,8,opt,name=description,proto3"`
}

func (x *acctTransactionRule) Reset()         { *x = acctTransactionRule{} }
func (x *acctTransactionRule) String() string { return fmt.Sprintf("%+v", *x) }
func (*acctTransactionRule) ProtoMessage()    {}

type acctListTransactionRulesRequest struct {
	ProductCode string `protobuf:"bytes,1,opt,name=product_code,json=productCode,proto3"`
}

func (x *acctListTransactionRulesRequest) Reset()         { *x = acctListTransactionRulesRequest{} }
func (x *acctListTransactionRulesRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*acctListTransactionRulesRequest) ProtoMessage()    {}

type acctListTransactionRulesResponse struct {
	Code    int32                  `protobuf:"varint,1,opt,name=code,proto3"`
	Message string                 `protobuf:"bytes,2,opt,name=message,proto3"`
	Items   []*acctTransactionRule `protobuf:"bytes,3,rep,name=items,proto3"`
}

func (x *acctListTransactionRulesResponse) Reset()         { *x = acctListTransactionRulesResponse{} }
func (x *acctListTransactionRulesResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*acctListTransactionRulesResponse) ProtoMessage()    {}

// ─── 客户端 ────────────────────────────────────────────────────────────────

// AccountingGRPCClient — 跟 accounting-system 的 gRPC TransactionService 互通.
//
// 跟旧 AccountingMetaClient (HTTP) 同形态接口, 替换简单 (engine adapter 只改 URL → conn).
type AccountingGRPCClient struct {
	cc      grpc.ClientConnInterface
	Timeout time.Duration

	// 元数据 cache (跟 HTTP 客户端一致, 5min)
	mu          sync.RWMutex
	typesCache  []*AccountTypeInfo
	typesExp    time.Time
	rulesCache  map[string][]*TransactionRule
	rulesExp    map[string]time.Time
}

// NewAccountingGRPCClient 用一个 gRPC ClientConn 构造客户端.
func NewAccountingGRPCClient(cc grpc.ClientConnInterface) *AccountingGRPCClient {
	return &AccountingGRPCClient{
		cc:         cc,
		Timeout:    3 * time.Second,
		rulesCache: map[string][]*TransactionRule{},
		rulesExp:   map[string]time.Time{},
	}
}

// CreateTransaction multi-leg 原子记账, 走 gRPC.
//
// 转换流程:
//   domain.TransactionRequest (split-payment 视角)
//     ↓ mapTransactionRequestToAcctReq
//   acctCreateTransactionRequest (wire 类型)
//     ↓ gRPC /accounting.v1.TransactionService/CreateTransaction
//   acctCreateTransactionResponse
//     ↓
//   CreateTransactionResponse (上层 API)
func (c *AccountingGRPCClient) CreateTransaction(ctx context.Context, req *domain.TransactionRequest) (*CreateTransactionResponse, error) {
	if req == nil {
		return nil, errors.New("nil request")
	}
	if req.OrderNo == "" || req.ProductCode == "" || req.EventCode == "" {
		return nil, errors.New("order_no / product_code / event_code required")
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	legs := make([]*acctTxnLeg, 0, len(req.Legs))
	for _, l := range req.Legs {
		legs = append(legs, &acctTxnLeg{
			EdgeFromNode:  l.EdgeFromNode,
			EdgeToNode:    l.EdgeToNode,
			FromAccountId: l.FromAccountID,
			ToAccountId:   l.ToAccountID,
			Amount:        l.Amount,
			Currency:      l.Currency,
		})
	}
	wireReq := &acctCreateTransactionRequest{
		OrderNo:     req.OrderNo,
		ProductCode: req.ProductCode,
		EventCode:   req.EventCode,
		Legs:        legs,
		Description: req.Description,
	}
	wireResp := new(acctCreateTransactionResponse)
	err := c.cc.Invoke(ctx, "/accounting.v1.TransactionService/CreateTransaction", wireReq, wireResp, grpc.StaticMethod())
	if err != nil {
		return nil, fmt.Errorf("CreateTransaction grpc: %w", err)
	}
	return &CreateTransactionResponse{
		OrderNo:      wireResp.OrderNo,
		Status:       int8(wireResp.Status),
		VoucherNo:    wireResp.VoucherNo,
		ErrorMessage: wireResp.ErrorMessage,
	}, nil
}

// ListAccountTypes 拉账户类型列表 (5min 缓存).
func (c *AccountingGRPCClient) ListAccountTypes(ctx context.Context) ([]*AccountTypeInfo, error) {
	c.mu.RLock()
	if time.Now().Before(c.typesExp) && len(c.typesCache) > 0 {
		out := c.typesCache
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	wireResp := new(acctListAccountTypesResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.TransactionService/ListAccountTypes",
		&acctListAccountTypesRequest{}, wireResp, grpc.StaticMethod()); err != nil {
		return nil, fmt.Errorf("ListAccountTypes grpc: %w", err)
	}
	out := make([]*AccountTypeInfo, 0, len(wireResp.Items))
	for _, it := range wireResp.Items {
		out = append(out, &AccountTypeInfo{
			AccountType:      it.AccountType,
			AccountTypeName:  it.AccountTypeName,
			OwnerType:        int(it.OwnerType),
			IsPlatform:       int(it.IsPlatform),
			BalanceDirection: it.BalanceDirection,
			Description:      it.Description,
		})
	}
	c.mu.Lock()
	c.typesCache = out
	c.typesExp = time.Now().Add(5 * time.Minute)
	c.mu.Unlock()
	return out, nil
}

// ListTransactionRules 按 product_code (= scenario) 拉规则; 空 = 全部. 5min cache.
func (c *AccountingGRPCClient) ListTransactionRules(ctx context.Context, productCode string) ([]*TransactionRule, error) {
	c.mu.RLock()
	if exp, ok := c.rulesExp[productCode]; ok && time.Now().Before(exp) {
		out := c.rulesCache[productCode]
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	wireResp := new(acctListTransactionRulesResponse)
	if err := c.cc.Invoke(ctx, "/accounting.v1.TransactionService/ListTransactionRules",
		&acctListTransactionRulesRequest{ProductCode: productCode}, wireResp, grpc.StaticMethod()); err != nil {
		return nil, fmt.Errorf("ListTransactionRules grpc: %w", err)
	}
	out := make([]*TransactionRule, 0, len(wireResp.Items))
	for _, it := range wireResp.Items {
		out = append(out, &TransactionRule{
			ID:              it.Id,
			ProductCode:     it.ProductCode,
			EventCode:       it.EventCode,
			DebitSubjectID:  it.DebitSubjectId,
			CreditSubjectID: it.CreditSubjectId,
			FromDirection:   it.FromDirection,
			ToDirection:     it.ToDirection,
			Description:     it.Description,
		})
	}
	c.mu.Lock()
	c.rulesCache[productCode] = out
	c.rulesExp[productCode] = time.Now().Add(5 * time.Minute)
	c.mu.Unlock()
	return out, nil
}

// InvalidateCache 强制刷新 (admin 改了 rule 之后调).
func (c *AccountingGRPCClient) InvalidateCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.typesCache = nil
	c.typesExp = time.Time{}
	c.rulesCache = map[string][]*TransactionRule{}
	c.rulesExp = map[string]time.Time{}
}
