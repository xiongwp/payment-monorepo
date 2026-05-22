// accounting_grpc.go — Kitex client → accounting-system TransactionService.
//
// 切 Kitex 后跟 gRPC wire 不互通; server side (accounting-system) 已同步切.
// 这里直接消费 transactionservice.Client, 不再手写 wire types — 复用 accounting-system
// 生成的 kitex_gen protobuf message types.

package clients

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/retry"
	"github.com/cloudwego/kitex/transport"
	"github.com/xiongwp/payment-util/kitexutil"

	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	transactionservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/transactionservice"

	"github.com/xiongwp/split-payment/internal/domain"
)

// parseAmountToMinor parses a leg amount string into minor units (int64).
// domain.TxnLeg.Amount 已经是 minor (translator 保证), 这里只做 trim + parse.
func parseAmountToMinor(s string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q: %w", s, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("amount %q must be ≥ 0", s)
	}
	return v, nil
}

// AccountingGRPCClient — Kitex client → accounting-system TransactionService.
//
// 跟旧 AccountingMetaClient (HTTP) 同形态接口, 替换简单 (engine adapter 只改构造).
// 名字保留 "GRPC" 是历史包袱 (split-payment 老代码到处引用), 实际是 Kitex.
type AccountingGRPCClient struct {
	cli     transactionservice.Client
	Timeout time.Duration

	// 元数据 cache (跟 HTTP 客户端一致, 5min)
	mu         sync.RWMutex
	typesCache []*AccountTypeInfo
	typesExp   time.Time
	rulesCache map[string][]*TransactionRule
	rulesExp   map[string]time.Time
}

// NewAccountingGRPCClient 用 endpoint 构造 Kitex client → accounting-system.
//
// 老签名 NewAccountingGRPCClient(cc grpc.ClientConnInterface) 已废. 新签名直接
// 拿 endpoint, Kitex 自带 connection pool + LB + keepalive.
//
// ETCD-5: 优先走 kitexutil.DefaultClientOptions ("accounting-service" 是 etcd
// 注册名, 同 accounting-system docker DNS). REGISTRY_ENDPOINTS 没配时回退到
// 静态 endpoint (老 dev / CLI 兼容); endpoint 字段允许空, 让 kitexutil 走 etcd.
func NewAccountingGRPCClient(endpoint string) (*AccountingGRPCClient, error) {
	opts := kitexutil.DefaultClientOptions("accounting-service")
	if endpoint != "" {
		// 显式指定的 endpoint 优先级最高 (CLI / 测试场景).
		opts = append(opts, client.WithHostPorts(endpoint))
	}
	// ⚡ 优化 #2: 禁掉 Kitex auto retry。
	// 默认 Kitex 在 timeout / connection-closed 等可重试错时会自动 retry 多次，
	// 让 5s timeout 实际变成 10-15s（叠加 1-2 次 retry）。压测时这放大效应会
	// 把 worker 全卡死。MaxRetryTimes=0 → 单次 timeout 立即失败。
	noRetryPolicy := &retry.FailurePolicy{
		StopPolicy: retry.StopPolicy{
			MaxRetryTimes:    0, // 关键：不重试
			MaxDurationMS:    0,
			DisableChainStop: false,
		},
	}

	opts = append(opts,
		// 强制 gRPC over HTTP/2 over TCP, 避开 Kitex netpoll 把 host:port 当 unix
		// socket 路径解读的 "dial unix ...: no such file or directory" 陷阱.
		client.WithTransportProtocol(transport.GRPC),
		// 2s inner timeout：loadtest 外层 3s，内层短 1s 让单 leg 先 fail-fast，
		// 整笔 TriggerEvent 不被一条慢 leg 拖死。retry 已禁掉，不会被放大。
		client.WithRPCTimeout(2*time.Second),
		client.WithFailureRetry(noRetryPolicy),
	)
	cli, err := transactionservice.NewClient("accounting-service", opts...)
	if err != nil {
		return nil, fmt.Errorf("kitex dial accounting-service: %w", err)
	}
	return &AccountingGRPCClient{
		cli:        cli,
		Timeout:    2 * time.Second,
		rulesCache: map[string][]*TransactionRule{},
		rulesExp:   map[string]time.Time{},
	}, nil
}

// CreateTransaction multi-leg 原子记账, 走 Kitex.
//
// 转换流程:
//
//	domain.TransactionRequest (split-payment 视角)
//	  → accountingv1.CreateTransactionRequest (kitex_gen wire 类型)
//	  → Kitex transactionservice.CreateTransaction
//	  → accountingv1.CreateTransactionResponse
//	  → CreateTransactionResponse (上层 API)
//
// 注: accounting-system 的 TransactionService 当前 .proto 是 skeleton (3 个核心字段:
// business_no / product_code / event_code + amount); 完整字段 (Legs[]/Description/...)
// 需要补齐 idl/accounting/v1/transaction.proto 后才能正确序列化.
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

	// TECH-DEBT-3 已把 Legs[] 加进 wire proto, 这里把 translator 派生的 leg
	// 复制到 wire 类型. accounting server 端按 leg 展开 2 行 AccountingEntry
	// (借方 + 贷方) 一起原子落账.
	wireLegs := make([]*accountingv1.TxnLeg, 0, len(req.Legs))
	for _, leg := range req.Legs {
		amt, perr := parseAmountToMinor(leg.Amount)
		if perr != nil {
			return nil, fmt.Errorf("leg %s→%s amount %q: %w", leg.EdgeFromNode, leg.EdgeToNode, leg.Amount, perr)
		}
		wireLegs = append(wireLegs, &accountingv1.TxnLeg{
			FromAccountNo: leg.FromAccountID,
			ToAccountNo:   leg.ToAccountID,
			AmountMinor:   amt,
			Currency:      leg.Currency,
			EdgeFromNode:  leg.EdgeFromNode,
			EdgeToNode:    leg.EdgeToNode,
		})
	}
	// 注意 BusinessNo vs OrderNo 语义分离：
	//   BusinessNo: 业务订单号 = chargeID（同一笔业务跨多 leg 一致），accounting 用作
	//               shard routing key → 必须用 req.BusinessNo，不是 req.OrderNo
	//   OrderNo  / IdempotencyKey: 单 leg 维度的请求 id（chargeID + event_code 拼成），
	//               accounting 用来去重幂等
	// 历史 bug：曾把 req.OrderNo 错塞进 BusinessNo，导致 OrderNo 里有下划线 / 字母
	// → accounting RouteByNumericStr ParseInt 失败 → 全部路由到 (0,0) → 单 shard 跑满。
	wireReq := &accountingv1.CreateTransactionRequest{
		BusinessNo:     req.BusinessNo,
		ProductCode:    req.ProductCode,
		EventCode:      req.EventCode,
		IdempotencyKey: req.OrderNo, // 业务 id 兼任幂等 key
		Remark:         req.Description,
		BusinessType:   req.BusinessType,
		Legs:           wireLegs,
	}
	wireResp, err := c.cli.CreateTransaction(ctx, wireReq)
	if err != nil {
		return nil, fmt.Errorf("CreateTransaction kitex: %w", err)
	}
	return &CreateTransactionResponse{
		OrderNo:      req.OrderNo,
		Status:       0, // skeleton proto 没有 Status int 字段, 用 string status 代替: posted/pending/failed
		VoucherNo:    wireResp.GetVoucherNo(),
		ErrorMessage: wireResp.GetErrorMessage(),
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
	wireResp, err := c.cli.ListAccountTypes(ctx, &accountingv1.ListAccountTypesRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListAccountTypes kitex: %w", err)
	}
	out := make([]*AccountTypeInfo, 0, len(wireResp.GetItems()))
	for _, it := range wireResp.GetItems() {
		out = append(out, &AccountTypeInfo{
			AccountType:     it.GetCode(),
			AccountTypeName: it.GetName(),
			// OwnerType / IsPlatform / BalanceDirection 在 skeleton proto 里没暴露, 留空.
			Description: it.GetDescription(),
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
	wireResp, err := c.cli.ListTransactionRules(ctx, &accountingv1.ListTransactionRulesRequest{
		ProductFilter: productCode,
	})
	if err != nil {
		return nil, fmt.Errorf("ListTransactionRules kitex: %w", err)
	}
	out := make([]*TransactionRule, 0, len(wireResp.GetItems()))
	for _, it := range wireResp.GetItems() {
		out = append(out, &TransactionRule{
			ProductCode:     it.GetProductCode(),
			EventCode:       it.GetEventCode(),
			DebitSubjectID:  it.GetDebitSubjectId(),
			CreditSubjectID: it.GetCreditSubjectId(),
			FromDirection:   it.GetFromDirection(),
			ToDirection:     it.GetToDirection(),
			// ID / Description 在 skeleton proto 里没暴露
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
