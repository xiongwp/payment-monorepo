package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/repository"

	"go.uber.org/zap"
)

// -----------------------------------------------------------------------
// Request / Response types — SP-AC-7 multi-leg per rule
//
// 语义重塑:
//   一个 CreateTransactionRequest = 一个 event_code = 一次原子账户操作
//   N 条 Leg → 上游 split-payment 图里同 event_code 的 N 条 edge
//   每条 Leg 一对 (from_account_id, to_account_id, amount)
//   from = debit, to = credit  (默认双式记账方向)
//
// 跟老 API 的区别:
//   - 不再传 from_party_id/from_party_type/to_party_id/to_party_type
//   - 不再用 (owner_type, party_id) → 查 account_no; account_no 由 caller 直接提供
//     (caller = split-payment translator, 它已经从 event.attributes 解析出来了)
//   - TransactionRule.debit_subject/credit_subject 退化成元数据/校验 hint, 不参与查询
// -----------------------------------------------------------------------

// CreateTransactionRequest 创建交易请求 (multi-leg).
type CreateTransactionRequest struct {
	OrderNo     string                 `json:"order_no"`     // 必填, 全局幂等键
	ProductCode string                 `json:"product_code"` // 必填
	EventCode   string                 `json:"event_code"`   // 必填 (= rule 名字)

	Legs []TxnLeg `json:"legs"` // ≥ 1 条 leg

	Description string                 `json:"description,omitempty"`
	MaxRetry    int                    `json:"max_retry,omitempty"` // 默认 3
	Extra       map[string]interface{} `json:"extra,omitempty"`
}

// TxnLeg 一条资金流 leg.
//
// from = 借方账户, to = 贷方账户 (双式记账标准方向).
// Amount 字符串保精度 (单位 minor).
type TxnLeg struct {
	EdgeFromNode  string `json:"edge_from_node,omitempty"`
	EdgeToNode    string `json:"edge_to_node,omitempty"`
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
}

// CreateTransactionResponse 创建交易响应.
type CreateTransactionResponse struct {
	OrderNo      string `json:"order_no"`
	Status       int8   `json:"status"`
	VoucherNo    string `json:"voucher_no,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// -----------------------------------------------------------------------
// Service interface
// -----------------------------------------------------------------------

type TransactionService interface {
	CreateTransaction(ctx context.Context, req *CreateTransactionRequest) (*CreateTransactionResponse, error)
	RetryTransaction(ctx context.Context, orderNo string) (*CreateTransactionResponse, error)
	GetTransaction(ctx context.Context, orderNo string) (*model.TransactionOrder, error)
}

// -----------------------------------------------------------------------
// Implementation
// -----------------------------------------------------------------------

type transactionService struct {
	ruleRepo          repository.TransactionRuleRepository
	orderRepo         repository.TransactionOrderRepository
	accountRepo       repository.AccountRepository
	businessTypeRepo  repository.AccountBusinessTypeRepository // SP-AC-7 resolveAccountRef 用
	accountingService AccountingService
	logger            *zap.Logger
}

// NewTransactionService 创建交易服务.
func NewTransactionService(
	ruleRepo repository.TransactionRuleRepository,
	orderRepo repository.TransactionOrderRepository,
	accountRepo repository.AccountRepository,
	businessTypeRepo repository.AccountBusinessTypeRepository,
	accountingService AccountingService,
	logger *zap.Logger,
) TransactionService {
	return &transactionService{
		ruleRepo:          ruleRepo,
		orderRepo:         orderRepo,
		accountRepo:       accountRepo,
		businessTypeRepo:  businessTypeRepo,
		accountingService: accountingService,
		logger:            logger,
	}
}

// orderExtraPayload — 持久化到 TransactionOrder.Extra (JSON) 的内容.
// 重试 / 审计时用来还原原始 Legs 等 multi-leg 信息.
type orderExtraPayload struct {
	Legs  []TxnLeg               `json:"legs,omitempty"`
	Extra map[string]interface{} `json:"extra,omitempty"`
}

// CreateTransaction 创建并执行一笔交易 (含幂等保护).
func (s *transactionService) CreateTransaction(ctx context.Context, req *CreateTransactionRequest) (*CreateTransactionResponse, error) {
	if err := s.validateRequest(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	totalAmount, currency, err := s.summarizeLegs(req.Legs)
	if err != nil {
		return nil, fmt.Errorf("invalid legs: %w", err)
	}

	// ── 1. 幂等检查 ──────────────────────────────────────────────────────
	existing, err := s.orderRepo.GetByOrderKey(ctx, req.OrderNo, "", req.OrderNo)
	if err != nil {
		return nil, fmt.Errorf("check idempotency failed: %w", err)
	}
	if existing != nil {
		switch existing.Status {
		case model.TransactionOrderStatusSuccess:
			s.logger.Info("order already succeeded, returning cached result",
				zap.String("orderNo", req.OrderNo))
			return &CreateTransactionResponse{
				OrderNo:   existing.OrderNo,
				Status:    existing.Status,
				VoucherNo: existing.VoucherNo,
			}, nil

		case model.TransactionOrderStatusProcessing:
			s.logger.Warn("order is being processed by another instance",
				zap.String("orderNo", req.OrderNo))
			return &CreateTransactionResponse{
				OrderNo: existing.OrderNo,
				Status:  existing.Status,
			}, nil

		case model.TransactionOrderStatusFailed:
			if existing.RetryCount >= existing.MaxRetryCount {
				return &CreateTransactionResponse{
					OrderNo:      existing.OrderNo,
					Status:       existing.Status,
					ErrorMessage: fmt.Sprintf("max retries exceeded (%d/%d): %s", existing.RetryCount, existing.MaxRetryCount, existing.ErrorMessage),
				}, nil
			}
			// 未达上限,继续走重试逻辑 (CAS 抢执行权)
		}
	}

	// ── 2. 创建或沿用 TransactionOrder ───────────────────────────────────
	if existing == nil {
		maxRetry := req.MaxRetry
		if maxRetry <= 0 {
			maxRetry = 3
		}
		payload := orderExtraPayload{Legs: req.Legs, Extra: req.Extra}
		extraJSON, _ := json.Marshal(payload)
		order := &model.TransactionOrder{
			OrderNo:       req.OrderNo,
			BusinessNo:    req.OrderNo,
			BusinessType:  "",
			ProductCode:   req.ProductCode,
			EventCode:     req.EventCode,
			Amount:        strconv.FormatInt(totalAmount, 10),
			Currency:      currency,
			Status:        model.TransactionOrderStatusPending,
			RetryCount:    0,
			MaxRetryCount: maxRetry,
			Description:   req.Description,
			Extra:         string(extraJSON),
		}
		if err := s.orderRepo.Create(ctx, order); err != nil {
			return nil, fmt.Errorf("create transaction order failed: %w", err)
		}
	}

	// ── 3. CAS 抢占执行权 (pending/failed → processing) ──────────────────
	affected, err := s.orderRepo.UpdateToProcessing(ctx, req.OrderNo, "", req.OrderNo)
	if err != nil {
		return nil, fmt.Errorf("update order to processing failed: %w", err)
	}
	if affected == 0 {
		s.logger.Warn("failed to acquire execution lock for order",
			zap.String("orderNo", req.OrderNo))
		return &CreateTransactionResponse{
			OrderNo: req.OrderNo,
			Status:  model.TransactionOrderStatusProcessing,
		}, nil
	}

	// ── 4. 执行记账 ──────────────────────────────────────────────────────
	voucherNo, execErr := s.executeBookkeeping(ctx, req, currency)

	// ── 5. 更新订单最终状态 ──────────────────────────────────────────────
	if execErr != nil {
		s.logger.Error("executeBookkeeping failed",
			zap.String("orderNo", req.OrderNo),
			zap.Error(execErr),
		)
		if updateErr := s.orderRepo.UpdateFailed(ctx, req.OrderNo, "", req.OrderNo, execErr.Error()); updateErr != nil {
			s.logger.Error("update order failed status error", zap.Error(updateErr))
		}
		return &CreateTransactionResponse{
			OrderNo:      req.OrderNo,
			Status:       model.TransactionOrderStatusFailed,
			ErrorMessage: execErr.Error(),
		}, nil
	}

	if updateErr := s.orderRepo.UpdateSuccess(ctx, req.OrderNo, "", req.OrderNo, voucherNo, ""); updateErr != nil {
		s.logger.Error("update order success status error", zap.Error(updateErr))
	}

	s.logger.Info("transaction completed",
		zap.String("orderNo", req.OrderNo),
		zap.String("voucherNo", voucherNo),
		zap.Int("legs", len(req.Legs)),
	)

	return &CreateTransactionResponse{
		OrderNo:   req.OrderNo,
		Status:    model.TransactionOrderStatusSuccess,
		VoucherNo: voucherNo,
	}, nil
}

// RetryTransaction 手动重试失败的订单.
//
// 从 TransactionOrder.Extra 里还原 Legs, 重新跑 CreateTransaction.
func (s *transactionService) RetryTransaction(ctx context.Context, orderNo string) (*CreateTransactionResponse, error) {
	order, err := s.orderRepo.GetByOrderKey(ctx, orderNo, "", orderNo)
	if err != nil {
		return nil, fmt.Errorf("query order failed: %w", err)
	}
	if order == nil {
		return nil, fmt.Errorf("order not found: %s", orderNo)
	}
	if order.Status != model.TransactionOrderStatusFailed {
		return &CreateTransactionResponse{
			OrderNo:      order.OrderNo,
			Status:       order.Status,
			ErrorMessage: "only failed orders can be retried",
		}, nil
	}
	if order.RetryCount >= order.MaxRetryCount {
		return &CreateTransactionResponse{
			OrderNo:      order.OrderNo,
			Status:       order.Status,
			ErrorMessage: fmt.Sprintf("max retries exceeded (%d/%d)", order.RetryCount, order.MaxRetryCount),
		}, nil
	}

	var payload orderExtraPayload
	if order.Extra != "" {
		if e := json.Unmarshal([]byte(order.Extra), &payload); e != nil {
			return nil, fmt.Errorf("decode order extra: %w", e)
		}
	}
	if len(payload.Legs) == 0 {
		return nil, fmt.Errorf("order %s has no legs to retry (extra empty or legacy schema)", orderNo)
	}

	req := &CreateTransactionRequest{
		OrderNo:     order.OrderNo,
		ProductCode: order.ProductCode,
		EventCode:   order.EventCode,
		Legs:        payload.Legs,
		Description: order.Description,
		MaxRetry:    order.MaxRetryCount,
		Extra:       payload.Extra,
	}
	return s.CreateTransaction(ctx, req)
}

// GetTransaction 查询订单状态.
func (s *transactionService) GetTransaction(ctx context.Context, orderNo string) (*model.TransactionOrder, error) {
	order, err := s.orderRepo.GetByOrderKey(ctx, orderNo, "", orderNo)
	if err != nil {
		return nil, fmt.Errorf("query order failed: %w", err)
	}
	if order == nil {
		return nil, fmt.Errorf("order not found: %s", orderNo)
	}
	return order, nil
}

// -----------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------

// executeBookkeeping 多 leg → AccountingEntry 列表 → 单次 DoubleEntryBooking.
//
// 流程:
//  1. (可选) 验证 rule (product_code, event_code) 存在
//  2. 每条 leg 产 2 个 entry: debit from_account, credit to_account
//  3. DoubleEntryBooking 一次原子落账 (一个 voucher_no 包含所有 leg)
func (s *transactionService) executeBookkeeping(ctx context.Context, req *CreateTransactionRequest, currency string) (string, error) {
	// 校验 rule 存在 (元数据校验,不参与查账)
	rules, err := s.ruleRepo.GetRulesByProductAndEvent(ctx, req.ProductCode, req.EventCode)
	if err != nil {
		return "", fmt.Errorf("lookup rule failed: %w", err)
	}
	if len(rules) == 0 {
		return "", fmt.Errorf("no rule found for product_code=%s event_code=%s",
			req.ProductCode, req.EventCode)
	}

	// 每条 leg → 2 个 raw entry (借 from, 贷 to),然后按 account_no 聚合
	// (accounting 内部 DoubleEntryBooking 不允许同一 account_no 在一笔里出现多次).
	//
	// SP-AC-7+ account ref 解析:
	//   leg.FromAccountID / ToAccountID 支持两种形态:
	//     1. 实际 account_no (e.g. "010100001-005")         — 直接用
	//     2. 业务语义 ref     (e.g. "USER_WALLET/42",
	//                             "PLATFORM_FEE_CLEARING/main") — 拆 prefix 当 business_type,
	//                                                            后缀当 user_id ("main"→0).
	//                                                            按 (user_id, account_business_type)
	//                                                            查 account 表拿真 account_no.
	//
	// 多 leg 共用 transit 账户: 净额 = sum(debit) - sum(credit). 净 0 → 跳过 (中转户进出相抵).
	type accSummary struct{ debit, credit int64 }
	agg := map[string]*accSummary{}
	resolved := map[string]string{} // ref → 真 account_no 缓存
	resolveRef := func(ref string) (string, error) {
		if v, ok := resolved[ref]; ok {
			return v, nil
		}
		v, err := s.resolveAccountRef(ctx, ref)
		if err != nil {
			return "", err
		}
		resolved[ref] = v
		return v, nil
	}
	for i, leg := range req.Legs {
		amount, err := strconv.ParseInt(leg.Amount, 10, 64)
		if err != nil {
			return "", fmt.Errorf("leg[%d] amount %q not int: %w", i, leg.Amount, err)
		}
		if amount <= 0 {
			return "", fmt.Errorf("leg[%d] amount must > 0, got %d", i, amount)
		}
		fromAcct, err := resolveRef(leg.FromAccountID)
		if err != nil {
			return "", fmt.Errorf("leg[%d] from ref %q: %w", i, leg.FromAccountID, err)
		}
		toAcct, err := resolveRef(leg.ToAccountID)
		if err != nil {
			return "", fmt.Errorf("leg[%d] to ref %q: %w", i, leg.ToAccountID, err)
		}
		if fromAcct == toAcct {
			return "", fmt.Errorf("leg[%d] from == to (%s); self-transfer not allowed", i, fromAcct)
		}
		if agg[fromAcct] == nil {
			agg[fromAcct] = &accSummary{}
		}
		agg[fromAcct].debit += amount
		if agg[toAcct] == nil {
			agg[toAcct] = &accSummary{}
		}
		agg[toAcct].credit += amount
	}
	entries := make([]AccountingEntry, 0, len(agg))
	for accNo, sum := range agg {
		net := sum.debit - sum.credit
		switch {
		case net > 0:
			entries = append(entries, AccountingEntry{
				AccountNo: accNo, DebitAmount: net, Description: req.Description,
			})
		case net < 0:
			entries = append(entries, AccountingEntry{
				AccountNo: accNo, CreditAmount: -net, Description: req.Description,
			})
			// net == 0: 中转户进出相抵, 不影响余额, 跳过 entry.
		}
	}

	businessType := mapEventCodeToBusinessType(req.EventCode)
	voucherNo, _, err := s.accountingService.DoubleEntryBooking(ctx, &DoubleEntryBookingRequest{
		RequestID:    req.OrderNo,
		BusinessNo:   req.OrderNo,
		BusinessType: businessType,
		Entries:      entries,
		Currency:     currency,
		Description:  req.Description,
	})
	return voucherNo, err
}

// summarizeLegs 校验 + 求和 + 统一币种.
//
// 规则:
//   - legs 非空
//   - 所有 leg 币种必须一致 (accounting 不支持跨币种原子操作)
//   - 总金额 = sum(leg.amount), 用于落 TransactionOrder.Amount 做摘要
func (s *transactionService) summarizeLegs(legs []TxnLeg) (int64, string, error) {
	if len(legs) == 0 {
		return 0, "", fmt.Errorf("legs cannot be empty")
	}
	currency := legs[0].Currency
	var total int64
	for i, leg := range legs {
		if leg.FromAccountID == "" {
			return 0, "", fmt.Errorf("leg[%d] from_account_id required", i)
		}
		if leg.ToAccountID == "" {
			return 0, "", fmt.Errorf("leg[%d] to_account_id required", i)
		}
		if leg.Currency == "" {
			return 0, "", fmt.Errorf("leg[%d] currency required", i)
		}
		if leg.Currency != currency {
			return 0, "", fmt.Errorf("leg[%d] currency %q != group %q (single-currency required)",
				i, leg.Currency, currency)
		}
		amt, err := strconv.ParseInt(leg.Amount, 10, 64)
		if err != nil {
			return 0, "", fmt.Errorf("leg[%d] amount %q not int: %w", i, leg.Amount, err)
		}
		total += amt
	}
	return total, currency, nil
}

// validateRequest 基本参数校验 (legs 内部校验在 summarizeLegs 里).
func (s *transactionService) validateRequest(req *CreateTransactionRequest) error {
	if req.OrderNo == "" {
		return fmt.Errorf("order_no is required")
	}
	if req.ProductCode == "" {
		return fmt.Errorf("product_code is required")
	}
	if req.EventCode == "" {
		return fmt.Errorf("event_code is required")
	}
	if len(req.Legs) == 0 {
		return fmt.Errorf("legs is required (≥ 1)")
	}
	return nil
}

// resolveAccountRef 把业务语义 ref 转成真实 account_no.
//
// 支持格式:
//   "<BUSINESS_TYPE_CODE>/<owner>"  → 查 (user_id=owner, account_business_type=<查 registry 得到的数字码>)
//
//   e.g. "PLATFORM_FEE_CLEARING/main"  → (user_id=0, business_type=11) → real account_no
//        "USER_WALLET/42"              → (user_id=42, business_type=USER_BALANCE 注册码)
//        "alipay_ch_fee/main"          → (user_id=0, business_type=ALIPAY_CH_FEE)
//
//   owner suffix:
//     "main" / 空 → user_id=0 (平台户)
//     纯数字 N    → user_id=N
//
// business_type_code 通过 account_business_type_info 表查得到对应数字 (管理 UI 注册时分配).
// 不再有硬编码映射, 注册新 business_type 不用改代码.
//
// 没 "/" 的 ref 当 raw account_no 直接透传 (兼容 caller 已知 account_no 的场景).
// registry 里找不到 code → 当 raw account_no 兜底 (避免阻塞).
func (s *transactionService) resolveAccountRef(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("empty account ref")
	}
	slash := strings.Index(ref, "/")
	if slash < 0 {
		return ref, nil // 当 raw account_no
	}
	code := ref[:slash]
	suffix := ref[slash+1:]

	// 查 business_type registry: code → 数字 business_type
	info, err := s.businessTypeRepo.GetByCode(ctx, code)
	if err != nil {
		return "", fmt.Errorf("lookup business_type code %q: %w", code, err)
	}
	if info == nil {
		// 没注册过的 code → 当 raw account_no 兜底
		return ref, nil
	}

	// owner 解析宽松: 提取末尾连续数字段.
	//   "main" / 空 → 0 (平台户)
	//   "42"        → 42
	//   "sub_42"    → 42 (前缀忽略)
	//   "abc"       → 0 (无数字, 兜底当平台户)
	var ownerID int64
	if suffix != "" && suffix != "main" {
		// 找末尾数字起点
		i := len(suffix)
		for i > 0 && suffix[i-1] >= '0' && suffix[i-1] <= '9' {
			i--
		}
		if i < len(suffix) {
			_, _ = fmt.Sscanf(suffix[i:], "%d", &ownerID)
		}
		// 全无数字 → ownerID 保持 0 (兼容 "main" 等占位)
	}

	acct, err := s.accountRepo.GetAccountByUserAndBusinessType(ctx, ownerID,
		model.AccountBusinessType(info.BusinessType))
	if err != nil {
		return "", fmt.Errorf("query account (user_id=%d, business_type=%d/%s): %w",
			ownerID, info.BusinessType, code, err)
	}
	if acct == nil {
		return "", fmt.Errorf("account not found: ref=%q resolved to (user_id=%d, business_type=%d/%s) but no row",
			ref, ownerID, info.BusinessType, code)
	}
	return acct.AccountNo, nil
}

// mapEventCodeToBusinessType event_code → 内部业务类型映射 (用于 voucher 归类).
func mapEventCodeToBusinessType(eventCode string) model.BusinessType {
	mapping := map[string]model.BusinessType{
		"DEPOSIT":      model.BusinessTypeDeposit,
		"WITHDRAW":     model.BusinessTypeWithdraw,
		"CHECKOUT_PAY": model.BusinessTypePayment,
		"PAY":          model.BusinessTypePayment,
		"REFUND":       model.BusinessTypeRefund,
		"TRANSFER":     model.BusinessTypeTransfer,
		"COMMISSION":   model.BusinessTypeCommission,
	}
	if bt, ok := mapping[eventCode]; ok {
		return bt
	}
	return model.BusinessTypeTransfer
}
