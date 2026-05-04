package service

import (
	"context"
	"fmt"

	commonutil "github.com/accounting-system/internal/common"
	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/repository"
	"go.uber.org/zap"
)

// AdjustmentService 调账服务（合规版）
//
// 调账走标准复式记账（HybridDoubleEntryBooking），不再单边修改余额：
//   - 必传 OffsetAccountNo（对方科目，通常是平台调差损益账户）
//   - 自动从 target.AccountCategory 推导借贷方向，offset 取反方向
//   - 享受 TCC 原子性 + Plan B finalized_at 切窗 + 试算平衡
//
// 设计原则：调账只是一种特殊的双分录记账，业务类型固定为 ADJUSTMENT；
// 借此和正常记账走完全相同的代码路径，避免出现破坏会计恒等式的旁路。
type AdjustmentService interface {
	// AdjustBalance 调账（修正账户余额，走双分录 + TCC）
	AdjustBalance(ctx context.Context, req *AdjustmentRequest) (*AdjustmentResponse, error)
}

// AdjustmentType 调账类型（仅作业务标签，不影响记账逻辑）
type AdjustmentType string

const (
	AdjustmentTypeCorrection AdjustmentType = "CORRECTION" // 余额修正
	AdjustmentTypeCompensate AdjustmentType = "COMPENSATE" // 补偿调账
	AdjustmentTypeManual     AdjustmentType = "MANUAL"     // 手工调账
)

// AdjustmentRequest 调账请求。Amount 单位：ISO 最小货币单位 × 100。
type AdjustmentRequest struct {
	// AccountNo 调账目标账户。
	AccountNo string
	// OffsetAccountNo 对方科目（通常为平台调差损益账户）。
	// 必填——单边调账违反会计恒等式，本服务拒绝处理。
	OffsetAccountNo string
	// AdjustmentType 业务标签。
	AdjustmentType AdjustmentType
	// Amount 调账金额（>0）。
	Amount int64
	// IsIncrease true=增加目标账户余额，false=减少。
	// 借贷方向由 target.AccountCategory 推导：
	//   - ASSET / EXPENSE   ：increase=DR，decrease=CR
	//   - LIABILITY / EQUITY / REVENUE：increase=CR，decrease=DR
	// offset 永远取反方向，保证 SUM(DR) == SUM(CR)。
	IsIncrease bool
	// Currency 必填，ISO 4217。
	Currency string
	// Reason 调账原因。
	Reason string
	// Operator 操作员。
	Operator string
	// ApprovalNo 审批单号（必填，权限审计依据）。
	ApprovalNo string
	// RelatedTxID 关联原交易（冲正/补偿场景，可空）。
	RelatedTxID string
	// RequestID 幂等键（必填）。重复传同一 RequestID 返回首次记账结果，不重复入账。
	RequestID string
}

// AdjustmentResponse 调账响应
type AdjustmentResponse struct {
	Success        bool
	TransactionID  string // target leg 的 transaction_id
	OffsetTxID     string // offset leg 的 transaction_id
	VoucherNo      string
	BalanceBefore  int64
	BalanceAfter   int64
	IdempotentHit  bool // true 表示重复 RequestID 命中已存在凭证
	ErrorMessage   string
}

type adjustmentService struct {
	accountingSvc AccountingService
	accountRepo   repository.AccountRepository
	logger        *zap.Logger
}

// NewAdjustmentService 创建调账服务
func NewAdjustmentService(
	accountingSvc AccountingService,
	accountRepo repository.AccountRepository,
	logger *zap.Logger,
) AdjustmentService {
	return &adjustmentService{
		accountingSvc: accountingSvc,
		accountRepo:   accountRepo,
		logger:        logger,
	}
}

// AdjustBalance 调账（双分录 + TCC + Plan B）
func (s *adjustmentService) AdjustBalance(ctx context.Context, req *AdjustmentRequest) (*AdjustmentResponse, error) {
	if err := s.validateAdjustment(ctx, req); err != nil {
		return &AdjustmentResponse{Success: false, ErrorMessage: err.Error()}, nil
	}

	// 读 target 账户拿 category，用于推导借贷方向；同时兜底 req.Currency。
	target, err := s.accountRepo.GetAccountByNo(ctx, req.AccountNo)
	if err != nil {
		return nil, fmt.Errorf("get target account %s: %w", req.AccountNo, err)
	}
	if target == nil {
		return &AdjustmentResponse{Success: false, ErrorMessage: fmt.Sprintf("account not found: %s", req.AccountNo)}, nil
	}
	// currency 兜底：账户已带币种。caller 没传 → 从账户取；传了 → 必须与账户一致
	// 防止误用（用 PHP 账户但请求里写 USD）。
	if req.Currency == "" {
		req.Currency = target.Currency
	} else if target.Currency != req.Currency {
		return &AdjustmentResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("currency mismatch: account=%s req=%s", target.Currency, req.Currency),
		}, nil
	}
	balanceBefore := target.Balance

	// 同步校验 offset 账户：不存在 / 币种不匹配会被 HybridDoubleEntryBooking 在 Try 阶段拒，
	// 这里提前查一次给出更清晰的错误，并避免和正常记账走同一个失败路径污染指标。
	offset, err := s.accountRepo.GetAccountByNo(ctx, req.OffsetAccountNo)
	if err != nil {
		return nil, fmt.Errorf("get offset account %s: %w", req.OffsetAccountNo, err)
	}
	if offset == nil {
		return &AdjustmentResponse{Success: false, ErrorMessage: fmt.Sprintf("offset account not found: %s", req.OffsetAccountNo)}, nil
	}
	if offset.Currency != req.Currency {
		return &AdjustmentResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("offset currency mismatch: account=%s req=%s", offset.Currency, req.Currency),
		}, nil
	}

	// 推导借贷方向：target 增加（IsIncrease=true）等价于
	//   ASSET/EXPENSE → 借方
	//   LIABILITY/EQUITY/REVENUE → 贷方
	// offset 永远取反方向。
	targetDebit := commonutil.IsAssetOrExpense(target.AccountCategory) == req.IsIncrease

	desc := fmt.Sprintf("调账[%s]: %s, 操作员: %s, 审批单号: %s",
		req.AdjustmentType, req.Reason, req.Operator, req.ApprovalNo)

	bookReq := &DoubleEntryBookingRequest{
		RequestID:    req.RequestID,
		BusinessNo:   req.ApprovalNo,
		BusinessType: model.BusinessType("ADJUSTMENT"),
		Currency:     req.Currency,
		Description:  desc,
		Entries: []AccountingEntry{
			s.makeEntry(req.AccountNo, req.Amount, targetDebit, "target"),
			s.makeEntry(req.OffsetAccountNo, req.Amount, !targetDebit, "offset"),
		},
	}

	voucherNo, txIDs, idemHit, err := s.accountingSvc.HybridDoubleEntryBooking(ctx, bookReq)
	if err != nil {
		s.logger.Error("adjustment booking failed",
			zap.String("requestID", req.RequestID),
			zap.String("accountNo", req.AccountNo),
			zap.String("offsetAccountNo", req.OffsetAccountNo),
			zap.Int64("amount", req.Amount),
			zap.Error(err))
		return &AdjustmentResponse{Success: false, ErrorMessage: err.Error()}, nil
	}

	// 重新读余额以填充响应（balance 已被 booking 更新；buffered 账户可能还在缓冲，
	// 这里反映 account 表实时值即可，调账侧不要求强一致 ending）
	after, err := s.accountRepo.GetAccountByNo(ctx, req.AccountNo)
	if err != nil {
		s.logger.Warn("read post-adjust balance failed (booking succeeded)",
			zap.String("voucherNo", voucherNo), zap.Error(err))
	}
	balanceAfter := balanceBefore
	if after != nil {
		balanceAfter = after.Balance
	}

	resp := &AdjustmentResponse{
		Success:       true,
		TransactionID: txIDs[0],
		VoucherNo:     voucherNo,
		BalanceBefore: balanceBefore,
		BalanceAfter:  balanceAfter,
		IdempotentHit: idemHit,
	}
	if len(txIDs) > 1 {
		resp.OffsetTxID = txIDs[1]
	}

	s.logAdjustmentAudit(ctx, req, resp.TransactionID, voucherNo, balanceBefore, balanceAfter)
	// 操作日志（运维定位用）只记 ID 类字段；金额 / 余额留给 audit 路径，避免
	// 在通用日志流里长期保留资金信息。
	s.logger.Info("adjustment completed",
		zap.String("requestID", req.RequestID),
		zap.String("voucherNo", voucherNo),
		zap.String("targetTxID", resp.TransactionID),
		zap.String("offsetTxID", resp.OffsetTxID),
		zap.Bool("idempotentHit", idemHit),
	)
	return resp, nil
}

func (s *adjustmentService) makeEntry(accountNo string, amount int64, isDebit bool, label string) AccountingEntry {
	e := AccountingEntry{AccountNo: accountNo, Description: label}
	if isDebit {
		e.DebitAmount = amount
	} else {
		e.CreditAmount = amount
	}
	return e
}

// validateAdjustment 校验调账参数。
//
// 历史上仅检查 4 个老字段（approval_no/operator/reason/amount）；现在 offset_account_no、
// request_id 是双分录调账的硬约束，缺一不可。currency 已不强制 —— 账户表自带
// currency，AdjustBalance 入口处用 target.Currency 兜底；caller 传了不同值会
// 在那里报 mismatch。先按老字段顺序校验是为了让既有单测继续通过。
func (s *adjustmentService) validateAdjustment(_ context.Context, req *AdjustmentRequest) error {
	if req.ApprovalNo == "" {
		return fmt.Errorf("approval number is required for adjustment")
	}
	if req.Operator == "" {
		return fmt.Errorf("operator is required for adjustment")
	}
	if req.Reason == "" {
		return fmt.Errorf("reason is required for adjustment")
	}
	if req.Amount <= 0 {
		return fmt.Errorf("adjustment amount must be greater than zero")
	}
	if req.AccountNo == "" {
		return fmt.Errorf("account_no is required")
	}
	if req.OffsetAccountNo == "" {
		return fmt.Errorf("offset_account_no is required (single-entry adjustments are forbidden)")
	}
	if req.AccountNo == req.OffsetAccountNo {
		return fmt.Errorf("offset_account_no must differ from account_no")
	}
	if req.RequestID == "" {
		return fmt.Errorf("request_id is required for idempotency")
	}
	return nil
}

// logAdjustmentAudit 写审计日志（应汇总到审计系统；此处仅记 zap）。
//
// 安全：金额 / 余额只出现在带 `audit_event="adjustment"` 标签的这一行；
// log shipping 应配置规则把 audit_event=* 路由到只读 + 严格访问控制的
// 独立审计 sink（CWE-532：sensitive info in log file）。运维流量看不到。
func (s *adjustmentService) logAdjustmentAudit(_ context.Context, req *AdjustmentRequest, transactionID, voucherNo string, balanceBefore, balanceAfter int64) {
	s.logger.Info("adjustment audit log",
		zap.String("audit_event", "adjustment"),
		zap.String("transactionID", transactionID),
		zap.String("voucherNo", voucherNo),
		zap.String("accountNo", req.AccountNo),
		zap.String("offsetAccountNo", req.OffsetAccountNo),
		zap.String("type", string(req.AdjustmentType)),
		zap.Int64("amount", req.Amount),
		zap.Bool("isIncrease", req.IsIncrease),
		zap.String("currency", req.Currency),
		zap.Int64("balanceBefore", balanceBefore),
		zap.Int64("balanceAfter", balanceAfter),
		zap.String("reason", req.Reason),
		zap.String("operator", req.Operator),
		zap.String("approvalNo", req.ApprovalNo),
		zap.String("relatedTxID", req.RelatedTxID),
	)
}
