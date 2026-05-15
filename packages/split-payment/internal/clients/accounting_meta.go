// accounting_meta.go — SP-AC-2 accounting-system meta + transaction HTTP-JSON client.
//
// 与现有 grpc AccountingClient (PostMovements) 平行, 走 HTTP-JSON 调:
//   GET  /admin/account_types           列 AccountTypeInfo
//   GET  /admin/transaction_rules?product=  列指定 scenario 的 rule
//   POST /admin/transactions            提交一笔 (product+event+amount) → accounting 内部按 rule 拆借贷
//
// 5min meta cache (AccountTypes / Rules 不常变, 高频读缓存避免 hammer).
package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"
)

// AccountTypeInfo 跟 accounting domain model 同形态, 这里只取 UI / 校验需要的字段.
type AccountTypeInfo struct {
	AccountType      string `json:"account_type"`       // e.g. "USER_WALLET"
	AccountTypeName  string `json:"account_type_name"`  // 中文名
	OwnerType        int    `json:"owner_type"`         // user/merchant/platform 数值码
	IsPlatform       int    `json:"is_platform"`        // 1 = 平台内部
	BalanceDirection string `json:"balance_direction"`  // C=贷方常态, D=借方常态
	Description      string `json:"description,omitempty"`
}

// TransactionRule.
type TransactionRule struct {
	ID              int64  `json:"id"`
	ProductCode     string `json:"product_code"`
	EventCode       string `json:"event_code"`
	DebitSubjectID  string `json:"debit_subject_id"`
	CreditSubjectID string `json:"credit_subject_id"`
	FromDirection   string `json:"from_direction"`
	ToDirection     string `json:"to_direction"`
	Description     string `json:"description,omitempty"`
}

// CreateTransactionResponse.
type CreateTransactionResponse struct {
	VoucherNo string   `json:"voucher_no"`
	TxIDs     []string `json:"tx_ids,omitempty"`
	Status    string   `json:"status"` // success / failed
	Error     string   `json:"error,omitempty"`
}

// AccountingMetaClient HTTP 客户端.
type AccountingMetaClient struct {
	BaseURL    string
	HTTPClient *http.Client
	AuthToken  string

	mu          sync.RWMutex
	typesCache  []*AccountTypeInfo
	typesExp    time.Time
	rulesCache  map[string][]*TransactionRule // product_code → rules
	rulesExp    map[string]time.Time
}

// NewAccountingMetaClient.
func NewAccountingMetaClient(baseURL, token string) *AccountingMetaClient {
	return &AccountingMetaClient{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 3 * time.Second},
		AuthToken:  token,
		rulesCache: map[string][]*TransactionRule{},
		rulesExp:   map[string]time.Time{},
	}
}

// ListAccountTypes 5min cache.
func (c *AccountingMetaClient) ListAccountTypes(ctx context.Context) ([]*AccountTypeInfo, error) {
	c.mu.RLock()
	if time.Now().Before(c.typesExp) && len(c.typesCache) > 0 {
		out := c.typesCache
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/admin/account_types", nil)
	c.addAuth(req)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list account types: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("account types upstream %d", resp.StatusCode)
	}
	var out []*AccountTypeInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode types: %w", err)
	}
	c.mu.Lock()
	c.typesCache = out
	c.typesExp = time.Now().Add(5 * time.Minute)
	c.mu.Unlock()
	return out, nil
}

// ListTransactionRules 按 product_code (= scenario) 拉所有规则. 5min cache.
//
// product_code 空 → 拉全部 (UI 总览用, 不推荐高频调).
func (c *AccountingMetaClient) ListTransactionRules(ctx context.Context, productCode string) ([]*TransactionRule, error) {
	c.mu.RLock()
	if exp, ok := c.rulesExp[productCode]; ok && time.Now().Before(exp) {
		out := c.rulesCache[productCode]
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	u := c.BaseURL + "/admin/transaction_rules"
	if productCode != "" {
		u += "?product=" + url.QueryEscape(productCode)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	c.addAuth(req)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("rules upstream %d", resp.StatusCode)
	}
	var out []*TransactionRule
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode rules: %w", err)
	}
	c.mu.Lock()
	c.rulesCache[productCode] = out
	c.rulesExp[productCode] = time.Now().Add(5 * time.Minute)
	c.mu.Unlock()
	return out, nil
}

// CreateTransaction 提交一笔交易给 accounting → 内部按 rule 自动拆借贷分录 + 落账.
//
// 这是 split-payment engine 调 accounting 的核心方法, 替代旧的 PostMovements.
// 失败可重试 (业务层用 OrderNo 做幂等键).
func (c *AccountingMetaClient) CreateTransaction(ctx context.Context, req *domain.TransactionRequest) (*CreateTransactionResponse, error) {
	if req.OrderNo == "" || req.ProductCode == "" || req.EventCode == "" {
		return nil, errors.New("CreateTransaction: order_no / product_code / event_code required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal req: %w", err)
	}
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/admin/transactions", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	c.addAuth(httpReq)
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create txn http: %w", err)
	}
	defer resp.Body.Close()
	var out CreateTransactionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode txn resp: %w", err)
	}
	if resp.StatusCode >= 400 {
		return &out, fmt.Errorf("create txn %d: %s", resp.StatusCode, out.Error)
	}
	return &out, nil
}

// addAuth.
func (c *AccountingMetaClient) addAuth(req *http.Request) {
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
}

// InvalidateCache 强制刷新 (admin 改了 rule 之后调).
func (c *AccountingMetaClient) InvalidateCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.typesCache = nil
	c.typesExp = time.Time{}
	c.rulesCache = map[string][]*TransactionRule{}
	c.rulesExp = map[string]time.Time{}
}
