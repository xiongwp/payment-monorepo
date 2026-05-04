package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/repository"

	"go.uber.org/zap"
)

// -----------------------------------------------------------------------
// Request / Response types
// -----------------------------------------------------------------------

// CreateTransactionRequest 创建交易请求
// order_no 由调用方提供，作为全局幂等键
type CreateTransactionRequest struct {
	OrderNo       string                 `json:"order_no"`        // 必填，幂等订单号
	ProductCode   string                 `json:"product_code"`    // 必填，产品编码
	EventCode     string                 `json:"event_code"`      // 必填，事件编码
	FromPartyID   int64                  `json:"from_party_id"`   // 资金出方 owner id
	FromPartyType string                 `json:"from_party_type"` // "user" | "merchant" | "platform"
	ToPartyID     int64                  `json:"to_party_id"`
	ToPartyType   string                 `json:"to_party_type"`
	Amount        int64                  `json:"amount"`
	Currency      string                 `json:"currency"`
	Description   string                 `json:"description"`
	MaxRetry      int                    `json:"max_retry"`  // 默认 3
	Extra         map[string]interface{} `json:"extra"`
}

// CreateTransactionResponse 创建交易响应
type CreateTransactionResponse struct {
	OrderNo      string `json:"order_no"`
	Status       int8   `json:"status"`
	VoucherNo    string `json:"voucher_no,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// -----------------------------------------------------------------------
// Service interface
// -----------------------------------------------------------------------

// TransactionService 交易门面服务
// 对外暴露：通过 product_code + event_code 驱动记账
// 内部依赖：TransactionRule 配置 + 内部 AccountingService
type TransactionService interface {
	// CreateTransaction 创建并执行一笔交易
	// 内置幂等：相同 order_no 只执行一次
	CreateTransaction(ctx context.Context, req *CreateTransactionRequest) (*CreateTransactionResponse, error)

	// RetryTransaction 手动重试失败订单
	RetryTransaction(ctx context.Context, orderNo string) (*CreateTransactionResponse, error)

	// GetTransaction 查询订单状态
	GetTransaction(ctx context.Context, orderNo string) (*model.TransactionOrder, error)
}

// -----------------------------------------------------------------------
// Implementation
// -----------------------------------------------------------------------

type transactionService struct {
	ruleRepo         repository.TransactionRuleRepository
	orderRepo        repository.TransactionOrderRepository
	accountRepo      repository.AccountRepository
	accountingService AccountingService
	logger           *zap.Logger
}

// NewTransactionService 创建交易服务
func NewTransactionService(
	ruleRepo repository.TransactionRuleRepository,
	orderRepo repository.TransactionOrderRepository,
	accountRepo repository.AccountRepository,
	accountingService AccountingService,
	logger *zap.Logger,
) TransactionService {
	return &transactionService{
		ruleRepo:          ruleRepo,
		orderRepo:         orderRepo,
		accountRepo:       accountRepo,
		accountingService: accountingService,
		logger:            logger,
	}
}

// CreateTransaction 创建并执行一笔交易（含幂等保护）
func (s *transactionService) CreateTransaction(ctx context.Context, req *CreateTransactionRequest) (*CreateTransactionResponse, error) {
	if err := s.validateRequest(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// ── 1. 幂等检查 ──────────────────────────────────────────────────────
	// transaction_service 以 orderNo 作为 businessNo，businessType 留空
	existing, err := s.orderRepo.GetByOrderKey(ctx, req.OrderNo, "", req.OrderNo)
	if err != nil {
		return nil, fmt.Errorf("check idempotency failed: %w", err)
	}
	if existing != nil {
		switch existing.Status {
		case model.TransactionOrderStatusSuccess:
			// 已成功，直接返回缓存结果
			s.logger.Info("order already succeeded, returning cached result",
				zap.String("orderNo", req.OrderNo))
			return &CreateTransactionResponse{
				OrderNo:   existing.OrderNo,
				Status:    existing.Status,
				VoucherNo: existing.VoucherNo,
			}, nil

		case model.TransactionOrderStatusProcessing:
			// 另一个进程正在处理
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
			// 未达上限，继续走重试逻辑（CAS 抢执行权）
		}
	}

	// ── 2. 创建或沿用 TransactionOrder ───────────────────────────────────
	if existing == nil {
		maxRetry := req.MaxRetry
		if maxRetry <= 0 {
			maxRetry = 3
		}
		extraJSON := ""
		if req.Extra != nil {
			if b, e := json.Marshal(req.Extra); e == nil {
				extraJSON = string(b)
			}
		}
		order := &model.TransactionOrder{
			OrderNo:       req.OrderNo,
			BusinessNo:    req.OrderNo, // transaction_service: businessNo = orderNo
			BusinessType:  "",
			ProductCode:   req.ProductCode,
			EventCode:     req.EventCode,
			FromPartyID:   req.FromPartyID,
			FromPartyType: req.FromPartyType,
			ToPartyID:     req.ToPartyID,
			ToPartyType:   req.ToPartyType,
			Amount:        strconv.FormatInt(req.Amount, 10),
			Currency:      req.Currency,
			Status:        model.TransactionOrderStatusPending,
			RetryCount:    0,
			MaxRetryCount: maxRetry,
			Description:   req.Description,
			Extra:         extraJSON,
		}
		if err := s.orderRepo.Create(ctx, order); err != nil {
			return nil, fmt.Errorf("create transaction order failed: %w", err)
		}
	}

	// ── 3. CAS 抢占执行权（pending/failed → processing）──────────────────
	affected, err := s.orderRepo.UpdateToProcessing(ctx, req.OrderNo, "", req.OrderNo)
	if err != nil {
		return nil, fmt.Errorf("update order to processing failed: %w", err)
	}
	if affected == 0 {
		// 被其他实例抢先
		s.logger.Warn("failed to acquire execution lock for order",
			zap.String("orderNo", req.OrderNo))
		return &CreateTransactionResponse{
			OrderNo: req.OrderNo,
			Status:  model.TransactionOrderStatusProcessing,
		}, nil
	}

	// ── 4. 执行记账 ───────────────────────────────────────────────────────
	voucherNo, execErr := s.executeBookkeeping(ctx, req)

	// ── 5. 更新订单最终状态 ───────────────────────────────────────────────
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
	)

	return &CreateTransactionResponse{
		OrderNo:   req.OrderNo,
		Status:    model.TransactionOrderStatusSuccess,
		VoucherNo: voucherNo,
	}, nil
}

// RetryTransaction 手动重试失败的订单
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

	amount, err := strconv.ParseInt(order.Amount, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid amount in order: %w", err)
	}

	req := &CreateTransactionRequest{
		OrderNo:       order.OrderNo,
		ProductCode:   order.ProductCode,
		EventCode:     order.EventCode,
		FromPartyID:   order.FromPartyID,
		FromPartyType: order.FromPartyType,
		ToPartyID:     order.ToPartyID,
		ToPartyType:   order.ToPartyType,
		Amount:        amount,
		Currency:      order.Currency,
		Description:   order.Description,
		MaxRetry:      order.MaxRetryCount,
	}
	return s.CreateTransaction(ctx, req)
}

// GetTransaction 查询订单状态
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

// executeBookkeeping 核心记账逻辑：规则解析 → 账户解析 → 复式记账
func (s *transactionService) executeBookkeeping(ctx context.Context, req *CreateTransactionRequest) (voucherNo string, err error) {
	// 1. 查询所有规则
	rules, err := s.ruleRepo.GetRulesByProductAndEvent(ctx, req.ProductCode, req.EventCode)
	if err != nil {
		return "", err
	}

	// 2. 逐条规则构建记账分录
	entries := make([]AccountingEntry, 0, len(rules)*2)
	for _, rule := range rules {
		ruleEntries, e := s.buildEntriesForRule(ctx, rule, req)
		if e != nil {
			return "", fmt.Errorf("rule[%d] build entries failed: %w", rule.ID, e)
		}
		entries = append(entries, ruleEntries...)
	}

	// 3. 调用内部复式记账
	businessType := mapEventCodeToBusinessType(req.EventCode)
	voucherNo, _, err = s.accountingService.DoubleEntryBooking(ctx, &DoubleEntryBookingRequest{
		BusinessNo:   req.OrderNo,
		BusinessType: businessType,
		Entries:      entries,
		Currency:     req.Currency,
		Description:  req.Description,
	})
	return voucherNo, err
}

// buildEntriesForRule 根据单条规则和请求构建一对借贷分录
//
// 规则字段语义：
//   - debit_subject_id  → 借方科目账户类型码
//   - credit_subject_id → 贷方科目账户类型码
//   - from_direction    → "debit" | "credit"：from方账户在本规则中扮演的角色
//   - to_direction      → "debit" | "credit"：to方账户在本规则中扮演的角色
//
// 账户解析：
//   - 哪个 direction == "debit"  → 使用 debit_subject_id 的 owner_type 查该方账户
//   - 哪个 direction == "credit" → 使用 credit_subject_id 的 owner_type 查该方账户
func (s *transactionService) buildEntriesForRule(ctx context.Context, rule *model.TransactionRule, req *CreateTransactionRequest) ([]AccountingEntry, error) {
	// 解析借方科目账户类型
	debitTypeInfo, err := s.ruleRepo.GetAccountTypeInfo(ctx, rule.DebitSubjectID)
	if err != nil {
		return nil, fmt.Errorf("get debit subject info failed (subject=%s): %w", rule.DebitSubjectID, err)
	}
	// 解析贷方科目账户类型
	creditTypeInfo, err := s.ruleRepo.GetAccountTypeInfo(ctx, rule.CreditSubjectID)
	if err != nil {
		return nil, fmt.Errorf("get credit subject info failed (subject=%s): %w", rule.CreditSubjectID, err)
	}

	// 根据方向确定 from方 和 to方 各自使用哪个科目
	// from_direction == "debit"  → from方账户 = debit_subject
	// from_direction == "credit" → from方账户 = credit_subject
	var fromSubjectInfo, toSubjectInfo *model.AccountTypeInfo
	var fromIsDebit, toIsDebit bool

	switch rule.FromDirection {
	case model.BookkeepingDirectionDebit:
		fromSubjectInfo = debitTypeInfo
		fromIsDebit = true
	case model.BookkeepingDirectionCredit:
		fromSubjectInfo = creditTypeInfo
		fromIsDebit = false
	default:
		return nil, fmt.Errorf("unknown from_direction: %s", rule.FromDirection)
	}

	switch rule.ToDirection {
	case model.BookkeepingDirectionDebit:
		toSubjectInfo = debitTypeInfo
		toIsDebit = true
	case model.BookkeepingDirectionCredit:
		toSubjectInfo = creditTypeInfo
		toIsDebit = false
	default:
		return nil, fmt.Errorf("unknown to_direction: %s", rule.ToDirection)
	}

	// 解析 from 方实际账户号
	fromAccountNo, err := s.resolveAccountNo(ctx, req.FromPartyID, req.FromPartyType, fromSubjectInfo)
	if err != nil {
		return nil, fmt.Errorf("resolve from-party account failed: %w", err)
	}
	// 解析 to 方实际账户号
	toAccountNo, err := s.resolveAccountNo(ctx, req.ToPartyID, req.ToPartyType, toSubjectInfo)
	if err != nil {
		return nil, fmt.Errorf("resolve to-party account failed: %w", err)
	}

	// 构建分录
	fromEntry := AccountingEntry{AccountNo: fromAccountNo, Description: req.Description}
	toEntry := AccountingEntry{AccountNo: toAccountNo, Description: req.Description}

	if fromIsDebit {
		fromEntry.DebitAmount = req.Amount
		fromEntry.CreditAmount = 0
	} else {
		fromEntry.CreditAmount = req.Amount
		fromEntry.DebitAmount = 0
	}
	if toIsDebit {
		toEntry.DebitAmount = req.Amount
		toEntry.CreditAmount = 0
	} else {
		toEntry.CreditAmount = req.Amount
		toEntry.DebitAmount = 0
	}

	return []AccountingEntry{fromEntry, toEntry}, nil
}

// resolveAccountNo 根据 partyID、partyType 和科目类型信息查找账户号
//
// 平台账户（ownerType == AccountTypePlatform | AccountTypeTransit）使用固定 ownerID=0，
// 不依赖请求中的 party_id。
func (s *transactionService) resolveAccountNo(ctx context.Context, partyID int64, partyType string, subjectInfo *model.AccountTypeInfo) (string, error) {
	ownerType := model.AccountType(subjectInfo.OwnerType)
	ownerID := partyID

	// 平台/中间账户不属于具体的 from/to 方
	if ownerType == model.AccountTypePlatform || ownerType == model.AccountTypeTransit {
		ownerID = 0
	}

	account, err := s.accountRepo.GetAccountByOwnerAndType(ctx, ownerID, ownerType)
	if err != nil {
		return "", fmt.Errorf("query account failed (ownerID=%d, type=%s): %w", ownerID, subjectInfo.AccountType, err)
	}
	if account == nil {
		return "", fmt.Errorf("account not found (ownerID=%d, accountType=%s)", ownerID, subjectInfo.AccountType)
	}
	return account.AccountNo, nil
}

// validateRequest 基本参数校验
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
	if req.Amount <= 0 {
		return fmt.Errorf("amount must be greater than zero")
	}
	if req.Currency == "" {
		return fmt.Errorf("currency is required")
	}
	return nil
}

// mapEventCodeToBusinessType event_code → 内部业务类型映射
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


