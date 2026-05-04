package service

// freeze_service.go — 资金冻结 / 解冻 服务
//
// 支持支付和提现场景下的资金冻结流程：
//
//  FreezeBalance        冻结账户可用余额（available_balance -= amount, frozen_balance += amount）
//                       用户看到的 balance 不变。
//
//  UnfreezeAndDebit     解冻并扣款（支付成功）：
//                       冻结账户 balance -= amount, frozen_balance -= amount；
//                       贷方账户执行正常 balance/available_balance 增加；
//                       生成记账凭证。
//
//  UnfreezeAndReturn    解冻并返还（支付失败）：
//                       available_balance += amount, frozen_balance -= amount。
//
// 幂等性：
//   - FreezeBalance 通过 (order_no, business_type, business_no) 三元组去重。
//   - UnfreezeAndDebit / UnfreezeAndReturn 依赖冻结订单状态：
//     FROZEN(1) → 执行；SUCCESS(2)/FAILED(3) → 幂等返回。
//
// 跨分片原子性：
//   - FreezeBalance：冻结订单与账户路由到同一 DB 分片（accountNo routing），
//     整个操作在单个 DB 事务中完成，保证绝对原子性。
//   - UnfreezeAndDebit：冻结账户更新 + 订单状态更新在同一 TX；贷方账户各自单独 TX。
//     贷方 TX 失败时记录错误日志，需人工处理（与 TCC Confirm 失败处理策略一致）。
//   - UnfreezeAndReturn：与 FreezeBalance 相同，单一 TX 保证原子性。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/idgen"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/repository"
	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// ─── Service types ────────────────────────────────────────────────────────────

// FreezeService 资金冻结服务接口
type FreezeService interface {
	// FreezeBalance 冻结账户可用余额。幂等，同一 order_no 重复调用返回相同结果。
	FreezeBalance(ctx context.Context, req *FreezeBalanceRequest) (*FreezeBalanceResult, error)

	// UnfreezeAndDebit 解冻并执行复式记账（支付成功路径）。
	// 冻结订单 FROZEN → SUCCESS。
	UnfreezeAndDebit(ctx context.Context, req *UnfreezeAndDebitRequest) (*UnfreezeAndDebitResult, error)

	// UnfreezeAndReturn 解冻并返还资金（支付失败路径）。
	// 冻结订单 FROZEN → FAILED。
	UnfreezeAndReturn(ctx context.Context, req *UnfreezeAndReturnRequest) error
}

// FreezeBalanceRequest 冻结请求
type FreezeBalanceRequest struct {
	OrderNo      string             // 幂等键（调用方提供，全局唯一）
	AccountNo    string             // 被冻结账户号
	BusinessNo   string             // 业务订单号（存储于冻结订单）
	BusinessType model.BusinessType // 业务类型
	Amount       int64              // 冻结金额（最小货币单位 × 100）
	Currency     string
	Description  string
}

// FreezeBalanceResult 冻结结果
type FreezeBalanceResult struct {
	OrderNo string // = req.OrderNo
}

// UnfreezeEntry 解冻并扣款的单条记账分录
type UnfreezeEntry struct {
	AccountNo    string
	DebitAmount  int64
	CreditAmount int64
	Description  string
}

// UnfreezeAndDebitRequest 解冻并扣款请求
type UnfreezeAndDebitRequest struct {
	// Freeze order lookup
	FreezeOrderNo      string             // 冻结订单号（= FreezeBalanceRequest.OrderNo）
	FreezeAccountNo    string             // 被冻结账户号（用于分片路由）
	FreezeBusinessNo   string             // 冻结订单的 business_no
	FreezeBusinessType model.BusinessType // 冻结订单的 business_type（= "freeze"）

	// Double-entry entries：必须包含且仅包含一条借方分录（accountNo = FreezeAccountNo，
	// debitAmount = 冻结金额）；其余为贷方分录。
	Entries     []UnfreezeEntry
	Currency    string
	Description string
}

// UnfreezeAndDebitResult 解冻并扣款结果
type UnfreezeAndDebitResult struct {
	VoucherNo      string
	TransactionIDs []string
}

// UnfreezeAndReturnRequest 解冻并返还请求
type UnfreezeAndReturnRequest struct {
	FreezeOrderNo      string             // 冻结订单号
	FreezeAccountNo    string             // 被冻结账户号（用于分片路由）
	FreezeBusinessNo   string             // 冻结订单的 business_no
	FreezeBusinessType model.BusinessType // 冻结订单的 business_type
}

// ─── freezeOrderExtra JSON structure ─────────────────────────────────────────

type freezeOrderExtra struct {
	FrozenAccountNo string `json:"frozen_account_no"`
}

// ─── Implementation ───────────────────────────────────────────────────────────

type freezeService struct {
	freezeOrderRepo repository.FreezeOrderRepository
	accountRepo     repository.AccountRepository
	transactionRepo repository.TransactionRepository
	voucherRepo     repository.VoucherRepository
	// compensateOutbox 当 Phase 2 失败、inline 补偿也失败时，落库为 PENDING 由
	// FreezeCompensateOutboxWorker 兜底重试到最终一致。可选（dev 不接也可跑）。
	compensateOutbox repository.FreezeCompensateOutboxRepository
	dbManager        *database.Manager
	router           *sharding.Router
	idGen            idgen.IDGenerator
	logger           *zap.Logger
}

// NewFreezeService 创建资金冻结服务
func NewFreezeService(
	freezeOrderRepo repository.FreezeOrderRepository,
	accountRepo repository.AccountRepository,
	transactionRepo repository.TransactionRepository,
	voucherRepo repository.VoucherRepository,
	compensateOutbox repository.FreezeCompensateOutboxRepository,
	dbManager *database.Manager,
	router *sharding.Router,
	idGen idgen.IDGenerator,
	logger *zap.Logger,
) FreezeService {
	return &freezeService{
		freezeOrderRepo:  freezeOrderRepo,
		accountRepo:      accountRepo,
		transactionRepo:  transactionRepo,
		voucherRepo:      voucherRepo,
		compensateOutbox: compensateOutbox,
		dbManager:       dbManager,
		router:          router,
		idGen:           idGen,
		logger:          logger,
	}
}

// ─── FreezeBalance ────────────────────────────────────────────────────────────

func (s *freezeService) FreezeBalance(ctx context.Context, req *FreezeBalanceRequest) (*FreezeBalanceResult, error) {
	if req.Amount <= 0 {
		return nil, fmt.Errorf("freeze: amount must be positive")
	}
	if req.AccountNo == "" || req.OrderNo == "" || req.BusinessNo == "" {
		return nil, fmt.Errorf("freeze: order_no, account_no and business_no are required")
	}

	// Route by accountNo: freeze order co-located with account on same DB shard.
	dbIdx, tableIdx := s.router.RouteByAccountNo(req.AccountNo)
	db, err := s.dbManager.GetDB(dbIdx)
	if err != nil {
		return nil, fmt.Errorf("freeze: get db[%d]: %w", dbIdx, err)
	}
	dbWithCtx := db.WithContext(ctx)

	// ── Idempotency check ──
	existing, err := s.freezeOrderRepo.GetByKey(ctx, dbWithCtx,
		req.OrderNo, model.TransactionOrderTypeFreeze, req.BusinessNo, tableIdx)
	if err != nil {
		return nil, fmt.Errorf("freeze: check existing order: %w", err)
	}
	if existing != nil {
		switch existing.Status {
		case model.TransactionOrderStatusProcessing: // 1 = FROZEN
			return &FreezeBalanceResult{OrderNo: existing.OrderNo}, nil
		case model.TransactionOrderStatusSuccess:
			return nil, fmt.Errorf("freeze order %s already debited (SUCCESS)", req.OrderNo)
		case model.TransactionOrderStatusFailed:
			return nil, fmt.Errorf("freeze order %s already returned (FAILED)", req.OrderNo)
		}
		// Status 0 (PENDING): previous TX rolled back, retry is safe.
	}

	// ── Build Extra ──
	extraBytes, _ := json.Marshal(freezeOrderExtra{FrozenAccountNo: req.AccountNo})

	// ── Atomic TX: create order + lock account + update account + set FROZEN ──
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, fmt.Errorf("freeze: begin tx: %w", tx.Error)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// Create freeze order (status=PENDING=0)
	order := &model.TransactionOrder{
		OrderNo:      req.OrderNo,
		BusinessNo:   req.BusinessNo,
		BusinessType: model.TransactionOrderTypeFreeze,
		Amount:       fmt.Sprintf("%d", req.Amount),
		Currency:     req.Currency,
		Status:       model.TransactionOrderStatusPending,
		Description:  req.Description,
		Extra:        string(extraBytes),
	}
	if createErr := s.freezeOrderRepo.Create(ctx, tx, order, tableIdx); createErr != nil {
		tx.Rollback()
		// On duplicate key: another parallel call already started; caller should retry.
		return nil, fmt.Errorf("freeze: create order: %w", createErr)
	}

	// Lock account
	account, err := s.accountRepo.GetAccountForUpdate(ctx, tx, req.AccountNo, dbIdx, tableIdx)
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("freeze: lock account %s: %w", req.AccountNo, err)
	}
	if account == nil {
		tx.Rollback()
		return nil, fmt.Errorf("freeze: account %s not found", req.AccountNo)
	}
	if account.AvailableBalance < req.Amount {
		tx.Rollback()
		return nil, fmt.Errorf("freeze: insufficient available balance (available=%d, requested=%d)",
			account.AvailableBalance, req.Amount)
	}

	// Update account balances
	accountTable := s.router.GetTableName("account", tableIdx)
	if err := tx.WithContext(ctx).Table(accountTable).
		Where("account_no = ?", req.AccountNo).
		Updates(map[string]interface{}{
			"available_balance": gorm.Expr("available_balance - ?", req.Amount),
			"frozen_balance":    gorm.Expr("frozen_balance + ?", req.Amount),
			"version":           gorm.Expr("version + 1"),
		}).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("freeze: update account balances: %w", err)
	}

	// CAS: PENDING(0) → FROZEN(1) — must succeed since we just created it
	affected, err := s.freezeOrderRepo.UpdateToFrozen(ctx, tx,
		req.OrderNo, model.TransactionOrderTypeFreeze, req.BusinessNo, tableIdx)
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("freeze: update order to frozen: %w", err)
	}
	if affected == 0 {
		tx.Rollback()
		return nil, fmt.Errorf("freeze: concurrent update conflict, please retry")
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("freeze: commit: %w", err)
	}

	s.logger.Info("freeze balance success",
		zap.String("orderNo", req.OrderNo),
		zap.String("accountNo", req.AccountNo),
		zap.Int64("amount", req.Amount),
	)
	return &FreezeBalanceResult{OrderNo: req.OrderNo}, nil
}

// ─── UnfreezeAndReturn ────────────────────────────────────────────────────────

func (s *freezeService) UnfreezeAndReturn(ctx context.Context, req *UnfreezeAndReturnRequest) error {
	if req.FreezeAccountNo == "" || req.FreezeOrderNo == "" || req.FreezeBusinessNo == "" {
		return fmt.Errorf("unfreeze-return: freeze_order_no, freeze_account_no and freeze_business_no are required")
	}

	dbIdx, tableIdx := s.router.RouteByAccountNo(req.FreezeAccountNo)
	db, err := s.dbManager.GetDB(dbIdx)
	if err != nil {
		return fmt.Errorf("unfreeze-return: get db[%d]: %w", dbIdx, err)
	}

	// Load freeze order
	freezeOrder, err := s.freezeOrderRepo.GetByKey(ctx, db.WithContext(ctx),
		req.FreezeOrderNo, model.TransactionOrderTypeFreeze, req.FreezeBusinessNo, tableIdx)
	if err != nil {
		return fmt.Errorf("unfreeze-return: load freeze order: %w", err)
	}
	if freezeOrder == nil {
		return fmt.Errorf("unfreeze-return: freeze order %s not found", req.FreezeOrderNo)
	}

	switch freezeOrder.Status {
	case model.TransactionOrderStatusFailed: // 3 = already returned
		return nil // idempotent
	case model.TransactionOrderStatusSuccess: // 2 = already debited
		return fmt.Errorf("unfreeze-return: freeze order %s already debited, cannot return", req.FreezeOrderNo)
	case model.TransactionOrderStatusPending: // 0 = not yet frozen (shouldn't happen)
		return fmt.Errorf("unfreeze-return: freeze order %s is not in FROZEN state", req.FreezeOrderNo)
	}
	// Status 1 (FROZEN): proceed

	// Parse frozen amount
	var frozenAmount int64
	if _, err := fmt.Sscanf(freezeOrder.Amount, "%d", &frozenAmount); err != nil {
		return fmt.Errorf("unfreeze-return: parse frozen amount %q: %w", freezeOrder.Amount, err)
	}

	// Parse frozen account from extra (defensive fallback to req.FreezeAccountNo)
	frozenAccountNo := req.FreezeAccountNo
	if freezeOrder.Extra != "" {
		var extra freezeOrderExtra
		if jsonErr := json.Unmarshal([]byte(freezeOrder.Extra), &extra); jsonErr == nil && extra.FrozenAccountNo != "" {
			frozenAccountNo = extra.FrozenAccountNo
		}
	}

	// ── Atomic TX: update account + update order ──
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("unfreeze-return: begin tx: %w", tx.Error)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// Lock account
	if _, lockErr := s.accountRepo.GetAccountForUpdate(ctx, tx, frozenAccountNo, dbIdx, tableIdx); lockErr != nil {
		tx.Rollback()
		return fmt.Errorf("unfreeze-return: lock account %s: %w", frozenAccountNo, lockErr)
	}

	// Restore balances: available_balance += amount, frozen_balance -= amount
	accountTable := s.router.GetTableName("account", tableIdx)
	if err := tx.WithContext(ctx).Table(accountTable).
		Where("account_no = ?", frozenAccountNo).
		Updates(map[string]interface{}{
			"available_balance": gorm.Expr("available_balance + ?", frozenAmount),
			"frozen_balance":    gorm.Expr("frozen_balance - ?", frozenAmount),
			"version":           gorm.Expr("version + 1"),
		}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("unfreeze-return: restore account balances: %w", err)
	}

	// Mark order FAILED (= funds returned)
	if err := s.freezeOrderRepo.UpdateFailed(ctx, tx,
		req.FreezeOrderNo, model.TransactionOrderTypeFreeze, req.FreezeBusinessNo,
		"unfreeze and return", tableIdx); err != nil {
		tx.Rollback()
		return fmt.Errorf("unfreeze-return: update order status: %w", err)
	}

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("unfreeze-return: commit: %w", err)
	}

	s.logger.Info("unfreeze and return success",
		zap.String("orderNo", req.FreezeOrderNo),
		zap.String("accountNo", frozenAccountNo),
		zap.Int64("amount", frozenAmount),
	)
	return nil
}

// ─── UnfreezeAndDebit ─────────────────────────────────────────────────────────

func (s *freezeService) UnfreezeAndDebit(ctx context.Context, req *UnfreezeAndDebitRequest) (*UnfreezeAndDebitResult, error) {
	if req.FreezeAccountNo == "" || req.FreezeOrderNo == "" || req.FreezeBusinessNo == "" {
		return nil, fmt.Errorf("unfreeze-debit: freeze_order_no, freeze_account_no and freeze_business_no are required")
	}
	if len(req.Entries) < 2 {
		return nil, fmt.Errorf("unfreeze-debit: at least 2 entries required for double-entry bookkeeping")
	}

	dbIdx, tableIdx := s.router.RouteByAccountNo(req.FreezeAccountNo)
	db, err := s.dbManager.GetDB(dbIdx)
	if err != nil {
		return nil, fmt.Errorf("unfreeze-debit: get db[%d]: %w", dbIdx, err)
	}

	// Load freeze order
	freezeOrder, err := s.freezeOrderRepo.GetByKey(ctx, db.WithContext(ctx),
		req.FreezeOrderNo, model.TransactionOrderTypeFreeze, req.FreezeBusinessNo, tableIdx)
	if err != nil {
		return nil, fmt.Errorf("unfreeze-debit: load freeze order: %w", err)
	}
	if freezeOrder == nil {
		return nil, fmt.Errorf("unfreeze-debit: freeze order %s not found", req.FreezeOrderNo)
	}

	switch freezeOrder.Status {
	case model.TransactionOrderStatusSuccess: // 2 = already debited → idempotent
		return &UnfreezeAndDebitResult{
			VoucherNo:      freezeOrder.VoucherNo,
			TransactionIDs: nil, // TxIDs not persisted separately for freeze; caller reconciles
		}, nil
	case model.TransactionOrderStatusFailed: // 3 = already returned
		return nil, fmt.Errorf("unfreeze-debit: freeze order %s already returned, cannot debit", req.FreezeOrderNo)
	case model.TransactionOrderStatusPending: // 0 = not yet frozen
		return nil, fmt.Errorf("unfreeze-debit: freeze order %s is not in FROZEN state", req.FreezeOrderNo)
	}
	// Status 1 (FROZEN): proceed

	// Parse frozen amount and account
	var frozenAmount int64
	if _, err := fmt.Sscanf(freezeOrder.Amount, "%d", &frozenAmount); err != nil {
		return nil, fmt.Errorf("unfreeze-debit: parse frozen amount %q: %w", freezeOrder.Amount, err)
	}
	frozenAccountNo := req.FreezeAccountNo
	if freezeOrder.Extra != "" {
		var extra freezeOrderExtra
		if jsonErr := json.Unmarshal([]byte(freezeOrder.Extra), &extra); jsonErr == nil && extra.FrozenAccountNo != "" {
			frozenAccountNo = extra.FrozenAccountNo
		}
	}

	// Validate entries: exactly one debit for frozenAccountNo with matching amount
	if err := validateUnfreezeEntries(req.Entries, frozenAccountNo, frozenAmount); err != nil {
		return nil, fmt.Errorf("unfreeze-debit: %w", err)
	}

	// Generate voucher number (routed same as frozen account for co-location)
	voucherNo, err := s.generateFreezeVoucherNo(ctx, dbIdx, tableIdx)
	if err != nil {
		return nil, fmt.Errorf("unfreeze-debit: generate voucher_no: %w", err)
	}

	now := time.Now()
	transactionDate := now.Format("2006-01-02")
	// cut_date 一次算定，propagate 到本次 unfreeze-debit 所有 entries
	// （frozen-debit + 普通 entries），保证 trial balance 按 cut_date 过滤时
	// 整笔同进同出。schema 是 NOT NULL DATE，零值会写成 '0000-00-00' 错位，
	// 导致借贷过滤后不平 ── 这是 ₱357.09 不平的根因之一。
	cutDate := transactionDate

	// Generate transaction IDs for all entries
	txIDs := make([]string, len(req.Entries))
	for i, entry := range req.Entries {
		txID, err := s.generateFreezeTxID(ctx, entry.AccountNo)
		if err != nil {
			return nil, fmt.Errorf("unfreeze-debit: generate tx_id for entry[%d]: %w", i, err)
		}
		txIDs[i] = txID
	}

	// Sort entries by accountNo for consistent lock ordering (deadlock prevention)
	type indexedEntry struct {
		idx   int
		entry UnfreezeEntry
	}
	sorted := make([]indexedEntry, len(req.Entries))
	for i, e := range req.Entries {
		sorted[i] = indexedEntry{i, e}
	}
	sort.Slice(sorted, func(a, b int) bool {
		return sorted[a].entry.AccountNo < sorted[b].entry.AccountNo
	})

	// ── Phase 1: Frozen account debit + order SUCCESS in one TX (same shard) ──
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, fmt.Errorf("unfreeze-debit: begin tx: %w", tx.Error)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var allTxIDs []string
	frozenAcctIdx := -1
	for _, ie := range sorted {
		if ie.entry.AccountNo == frozenAccountNo && ie.entry.DebitAmount == frozenAmount {
			frozenAcctIdx = ie.idx
			break
		}
	}

	// Lock frozen account
	frozenAcct, err := s.accountRepo.GetAccountForUpdate(ctx, tx, frozenAccountNo, dbIdx, tableIdx)
	if err != nil || frozenAcct == nil {
		tx.Rollback()
		if frozenAcct == nil {
			err = errors.New("account not found")
		}
		return nil, fmt.Errorf("unfreeze-debit: lock frozen account: %w", err)
	}

	// Apply debit to frozen account: balance -= amount, frozen_balance -= amount
	// (available_balance was already reduced at freeze time)
	balanceBefore := frozenAcct.Balance
	balanceAfter := balanceBefore - frozenAmount
	accountTable := s.router.GetTableName("account", tableIdx)
	if err := tx.WithContext(ctx).Table(accountTable).
		Where("account_no = ?", frozenAccountNo).
		Updates(map[string]interface{}{
			"balance":        gorm.Expr("balance - ?", frozenAmount),
			"frozen_balance": gorm.Expr("frozen_balance - ?", frozenAmount),
			"version":        gorm.Expr("version + 1"),
		}).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("unfreeze-debit: apply debit to frozen account: %w", err)
	}

	// Write transaction record for frozen account debit
	frozenEntryDesc := req.Entries[frozenAcctIdx].Description
	if frozenEntryDesc == "" {
		frozenEntryDesc = req.Description
	}
	frozenTxRecord := &model.AccountTransaction{
		TransactionID:   txIDs[frozenAcctIdx],
		AccountNo:       frozenAccountNo,
		BusinessNo:      req.FreezeBusinessNo,
		BusinessType:    model.BusinessType(req.FreezeBusinessType),
		DebitAmount:     frozenAmount,
		CreditAmount:    0,
		BalanceBefore:   balanceBefore,
		BalanceAfter:    balanceAfter,
		BookingType:     model.TransactionBookingTypeSync,
		Currency:        req.Currency,
		TransactionDate: transactionDate,
		TransactionTime: now,
		// cut_date 必须设：DB 列 NOT NULL，零值会写成 0000-00-00 让 trial
		// balance 按 cut_date 过滤时该 entry 错失，与同 voucher 其它 entry
		// 落在不同日期，借贷不平。一笔 unfreeze-debit 操作内的所有 entries
		// 共享同一 cutDate（在方法入口算一次）。
		CutDate:         cutDate,
		Status:          model.TransactionStatusSuccess,
	}
	descStr := frozenEntryDesc
	frozenTxRecord.ParentTransactionID = &voucherNo
	frozenTxRecord.Description = &descStr
	if err := s.transactionRepo.CreateTransaction(ctx, tx, frozenTxRecord, dbIdx, tableIdx); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("unfreeze-debit: create transaction for frozen account: %w", err)
	}
	allTxIDs = append(allTxIDs, txIDs[frozenAcctIdx])

	// Create accounting voucher (co-located with frozen account)
	var totalDebit, totalCredit int64
	for _, e := range req.Entries {
		totalDebit += e.DebitAmount
		totalCredit += e.CreditAmount
	}
	voucherDesc := req.Description
	voucher := &model.AccountingVoucher{
		VoucherNo:    voucherNo,
		BusinessNo:   req.FreezeBusinessNo,
		BusinessType: model.BusinessType(req.FreezeBusinessType),
		TotalDebit:   totalDebit,
		TotalCredit:  totalCredit,
		Currency:     req.Currency,
		VoucherDate:  transactionDate,
		Status:       1,
		Description:  &voucherDesc,
	}
	if err := s.voucherRepo.Create(ctx, tx, voucher, dbIdx, tableIdx); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("unfreeze-debit: create voucher: %w", err)
	}

	// Mark freeze order SUCCESS
	if err := s.freezeOrderRepo.UpdateSuccess(ctx, tx,
		req.FreezeOrderNo, model.TransactionOrderTypeFreeze, req.FreezeBusinessNo,
		voucherNo, tableIdx); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("unfreeze-debit: mark freeze order SUCCESS: %w", err)
	}

	// 同 tx INSERT 补偿 outbox PENDING：与 frozen-debit + freeze_order SUCCESS
	// 原子提交。如果后续 Phase 2 + inline 补偿都没把状态改成 DONE，那 worker
	// 60s 后会扫到这条 PENDING 并兜底执行补偿，保证最终一致性。
	// 只在 compensateOutbox 注入了的情况下写（dev 单元测试可不接）。
	if s.compensateOutbox != nil {
		// 收集 credit entries（除 frozen account 外的所有 entries）作为 payload。
		// SuccessfulTxIDs 留空，等 Phase 2 进展后由 inline 路径管理（worker 看到
		// PENDING 时假定全部需要尝试 → 工作量稍大但正确性 OK，因为
		// applyCreditEntry 内部对已存在的 transaction_id 做 ON DUPLICATE 跳过）。
		credits := make([]model.FreezeCompensateEntry, 0, len(req.Entries)-1)
		for _, e := range req.Entries {
			if e.AccountNo == frozenAccountNo {
				continue
			}
			credits = append(credits, model.FreezeCompensateEntry{
				AccountNo:    e.AccountNo,
				DebitAmount:  e.DebitAmount,
				CreditAmount: e.CreditAmount,
				Description:  e.Description,
				TxID:         txIDs[indexOfEntry(req.Entries, e)],
			})
		}
		payload := &model.FreezeCompensatePayload{
			FreezeAccountNo:    frozenAccountNo,
			FreezeAmount:       frozenAmount,
			FreezeBusinessNo:   req.FreezeBusinessNo,
			FreezeBusinessType: string(req.FreezeBusinessType),
			Currency:           req.Currency,
			Description:        req.Description,
			TransactionDate:    transactionDate,
			CutDate:            cutDate,
			NowUnix:            now.Unix(),
			CreditEntries:      credits,
		}
		payloadStr, mErr := payload.Marshal()
		if mErr != nil {
			tx.Rollback()
			return nil, fmt.Errorf("unfreeze-debit: marshal compensate payload: %w", mErr)
		}
		outboxRow := &model.FreezeCompensateOutbox{
			VoucherNo:     voucherNo,
			FreezeOrderNo: req.FreezeOrderNo,
			Payload:       payloadStr,
			Status:        model.FreezeCompensateStatusPending,
		}
		if oErr := s.compensateOutbox.Insert(ctx, tx, outboxRow); oErr != nil {
			tx.Rollback()
			return nil, fmt.Errorf("unfreeze-debit: insert compensate outbox: %w", oErr)
		}
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("unfreeze-debit: commit frozen account tx: %w", err)
	}

	// ── Phase 2: Credit entries (independent per-shard TXs) ──
	//
	// 资金安全：Phase 1 (frozen-debit + freeze_order SUCCESS) 已 commit。
	// Phase 2 跨分片每个 entry 各自独立 tx。任一失败必须**对已成功的 entry +
	// Phase 1 做补偿**，否则会出现"已 debit 一边，没 credit 另一边"的资损。
	//
	// 实现：compensating cancel
	//   - 每条成功的 credit entry → 在其分片写 reverse_debit transaction 反向冲账，
	//     account.balance -= 原 credit_amount，并对该 entry 写一条新的 transaction
	//     row（status=SUCCESS）记审计 trail
	//   - 已 commit 的 Phase 1 frozen-debit → 在 frozen account 的分片写 reverse
	//     transaction：balance += A，frozen_balance += A，并把 freeze_order 状态
	//     从 SUCCESS 改回 FROZEN（让上层可以重试整笔操作）
	//
	// 极端场景：补偿本身也失败时 → log CRITICAL + metric，进入需要人工介入的
	// 不一致态。这是 vs 之前 silent fail 的明确进步：fail-loud + 自动补偿。
	type successfulCredit struct {
		entry UnfreezeEntry
		txID  string
		idx   int
	}
	var successfulCredits []successfulCredit
	var creditFailures []error
	var failedAccounts []string
	for _, ie := range sorted {
		if ie.entry.AccountNo == frozenAccountNo {
			continue // already handled above
		}
		entry := ie.entry
		creditTxID := txIDs[ie.idx]
		if err := s.applyCreditEntry(ctx, entry, creditTxID, voucherNo, req, transactionDate, cutDate, now); err != nil {
			s.logger.Error("CRITICAL: unfreeze-debit credit entry failed — initiating compensation",
				zap.String("voucherNo", voucherNo),
				zap.String("accountNo", entry.AccountNo),
				zap.Int64("amount", entry.DebitAmount+entry.CreditAmount),
				zap.Error(err),
			)
			creditFailures = append(creditFailures, fmt.Errorf("credit %s: %w", entry.AccountNo, err))
			failedAccounts = append(failedAccounts, entry.AccountNo)
			break // 任一失败立即停止，进入补偿
		}
		successfulCredits = append(successfulCredits, successfulCredit{
			entry: entry, txID: creditTxID, idx: ie.idx,
		})
		allTxIDs = append(allTxIDs, creditTxID)
	}

	if len(creditFailures) > 0 {
		// 补偿：先 reverse 已成功的 credit entries，再 reverse Phase 1
		var compensateErrs []error
		for _, sc := range successfulCredits {
			if err := s.compensateCreditEntry(ctx, sc.entry, sc.txID, voucherNo, req, transactionDate, cutDate, now); err != nil {
				compensateErrs = append(compensateErrs, fmt.Errorf("compensate credit %s: %w", sc.entry.AccountNo, err))
				s.logger.Error("CRITICAL: compensating credit reverse failed — manual intervention required",
					zap.String("voucherNo", voucherNo),
					zap.String("accountNo", sc.entry.AccountNo),
					zap.Error(err))
			}
		}
		if err := s.compensateFrozenDebit(ctx, req, frozenAccountNo, frozenAmount, dbIdx, tableIdx,
			voucherNo, transactionDate, cutDate, now); err != nil {
			compensateErrs = append(compensateErrs, fmt.Errorf("compensate frozen-debit: %w", err))
			s.logger.Error("CRITICAL: compensating frozen-debit reverse failed — manual intervention required",
				zap.String("voucherNo", voucherNo),
				zap.String("freezeOrderNo", req.FreezeOrderNo),
				zap.Error(err))
		}
		if len(compensateErrs) > 0 {
			return nil, fmt.Errorf("unfreeze-debit: %d credit entries failed (accounts %v) AND compensation also failed: %w; original errors: %w",
				len(creditFailures), failedAccounts,
				errors.Join(compensateErrs...), errors.Join(creditFailures...))
		}
		// 补偿全部成功 → CAS outbox PENDING → DONE 防 worker 重复补偿。
		if s.compensateOutbox != nil {
			if _, mErr := s.compensateOutbox.MarkDone(ctx, voucherNo); mErr != nil {
				s.logger.Error("CRITICAL: inline compensate succeeded but MarkDone outbox failed — worker may double-compensate",
					zap.String("voucherNo", voucherNo), zap.Error(mErr))
			}
		}
		// 整笔操作回到 Phase 1 之前的状态，caller 可以安全重试。
		return nil, fmt.Errorf("unfreeze-debit: %d credit entries failed (accounts %v), all changes reverted; safe to retry: %w",
			len(creditFailures), failedAccounts, errors.Join(creditFailures...))
	}

	// 全部成功 → CAS 把补偿 outbox PENDING → DONE，避免 worker 后续误抢补偿。
	if s.compensateOutbox != nil {
		if ok, err := s.compensateOutbox.MarkDone(ctx, voucherNo); err != nil {
			s.logger.Warn("unfreeze-debit: MarkDone outbox failed (worker may unnecessarily retry; harmless idempotent)",
				zap.String("voucherNo", voucherNo), zap.Error(err))
		} else if !ok {
			// 0 rows affected：worker 抢先 ClaimPending 了。caller 视角 Phase 2 全成功，
			// worker 看到的应是更新后的状态（已落 transaction record）→ worker 反向时
			// applyCreditEntry 的 ON DUPLICATE KEY 会跳过反向（已落库），但 worker 的反向
			// transaction 仍会写一条 → 净额抵消 OK，但有冗余审计行。打 WARN 便于诊断。
			s.logger.Warn("unfreeze-debit: outbox not in PENDING when caller marked DONE (worker likely claimed; review audit trail)",
				zap.String("voucherNo", voucherNo))
		}
	}

	s.logger.Info("unfreeze and debit success",
		zap.String("freezeOrderNo", req.FreezeOrderNo),
		zap.String("voucherNo", voucherNo),
		zap.String("frozenAccountNo", frozenAccountNo),
		zap.Int64("amount", frozenAmount),
	)
	return &UnfreezeAndDebitResult{
		VoucherNo:      voucherNo,
		TransactionIDs: allTxIDs,
	}, nil
}

// indexOfEntry 找到 entry 在 req.Entries 中的下标（用于查 txIDs[i]）。
func indexOfEntry(entries []UnfreezeEntry, target UnfreezeEntry) int {
	for i, e := range entries {
		if e.AccountNo == target.AccountNo && e.DebitAmount == target.DebitAmount && e.CreditAmount == target.CreditAmount {
			return i
		}
	}
	return -1
}

// applyCreditEntry applies a credit to one account in its own DB transaction.
func (s *freezeService) applyCreditEntry(
	ctx context.Context,
	entry UnfreezeEntry,
	txID, voucherNo string,
	req *UnfreezeAndDebitRequest,
	transactionDate string,
	cutDate string,
	now time.Time,
) error {
	entryDBIdx, entryTableIdx := s.router.RouteByAccountNo(entry.AccountNo)
	entryDB, err := s.dbManager.GetDB(entryDBIdx)
	if err != nil {
		return fmt.Errorf("get db[%d]: %w", entryDBIdx, err)
	}

	tx := entryDB.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin tx: %w", tx.Error)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	acct, err := s.accountRepo.GetAccountForUpdate(ctx, tx, entry.AccountNo, entryDBIdx, entryTableIdx)
	if err != nil || acct == nil {
		tx.Rollback()
		if acct == nil {
			err = fmt.Errorf("account not found")
		}
		return err
	}

	// Credit: balance += creditAmount, available_balance += creditAmount
	creditAmount := entry.CreditAmount
	balanceBefore := acct.Balance
	balanceAfter := balanceBefore + creditAmount
	accountTable := s.router.GetTableName("account", entryTableIdx)
	if err := tx.WithContext(ctx).Table(accountTable).
		Where("account_no = ?", entry.AccountNo).
		Updates(map[string]interface{}{
			"balance":           gorm.Expr("balance + ?", creditAmount),
			"available_balance": gorm.Expr("available_balance + ?", creditAmount),
			"version":           gorm.Expr("version + 1"),
		}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("apply credit balance: %w", err)
	}

	entryDesc := entry.Description
	if entryDesc == "" {
		entryDesc = req.Description
	}
	txRecord := &model.AccountTransaction{
		TransactionID:   txID,
		AccountNo:       entry.AccountNo,
		BusinessNo:      req.FreezeBusinessNo,
		BusinessType:    model.BusinessType(req.FreezeBusinessType),
		DebitAmount:     0,
		CreditAmount:    creditAmount,
		BalanceBefore:   balanceBefore,
		BalanceAfter:    balanceAfter,
		BookingType:     model.TransactionBookingTypeSync,
		Currency:        req.Currency,
		TransactionDate: transactionDate,
		TransactionTime: now,
		// cut_date 必须设；同 voucher 所有 entry 共享同一 cutDate。
		CutDate:         cutDate,
		Status:          model.TransactionStatusSuccess,
	}
	txRecord.ParentTransactionID = &voucherNo
	txRecord.Description = &entryDesc
	if err := s.transactionRepo.CreateTransaction(ctx, tx, txRecord, entryDBIdx, entryTableIdx); err != nil {
		tx.Rollback()
		return fmt.Errorf("create transaction record: %w", err)
	}

	return tx.Commit().Error
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// validateUnfreezeEntries checks that entries are valid for UnfreezeAndDebit:
//   - Exactly one debit entry for frozenAccountNo with debitAmount == frozenAmount
//   - Total debit == total credit
func validateUnfreezeEntries(entries []UnfreezeEntry, frozenAccountNo string, frozenAmount int64) error {
	var totalDebit, totalCredit int64
	frozenDebitCount := 0
	for _, e := range entries {
		if e.DebitAmount != 0 && e.CreditAmount != 0 {
			return fmt.Errorf("entry for %s has both debit and credit", e.AccountNo)
		}
		if e.DebitAmount == 0 && e.CreditAmount == 0 {
			return fmt.Errorf("entry for %s has neither debit nor credit", e.AccountNo)
		}
		totalDebit += e.DebitAmount
		totalCredit += e.CreditAmount
		if e.AccountNo == frozenAccountNo && e.DebitAmount == frozenAmount {
			frozenDebitCount++
		}
	}
	if frozenDebitCount == 0 {
		return fmt.Errorf("entries must include a debit of %d for frozen account %s", frozenAmount, frozenAccountNo)
	}
	if frozenDebitCount > 1 {
		return fmt.Errorf("entries must have exactly one debit for frozen account %s", frozenAccountNo)
	}
	if totalDebit != totalCredit {
		return fmt.Errorf("debit/credit imbalance: totalDebit=%d, totalCredit=%d", totalDebit, totalCredit)
	}
	return nil
}

// compensateCreditEntry 反向冲账某条已成功的 Phase 2 credit entry。
//
// 原始 entry: account.balance += credit_amount（已 commit）
// 反向 entry: account.balance -= credit_amount（在同一分片新 tx 写）
// 同时写一条 reverse transaction record（debit_amount = 原 credit_amount，
// status=SUCCESS, parent_transaction_id 关联原 voucher_no）保证 trial balance
// 仍然有完整审计 trail（原 entry 仍在 + 反向 entry 也在 → 净额抵消）。
func (s *freezeService) compensateCreditEntry(
	ctx context.Context,
	entry UnfreezeEntry,
	originalTxID, voucherNo string,
	req *UnfreezeAndDebitRequest,
	transactionDate, cutDate string,
	now time.Time,
) error {
	dbIdx, tableIdx := s.router.RouteByAccountNo(entry.AccountNo)
	db, err := s.dbManager.GetDB(dbIdx)
	if err != nil {
		return fmt.Errorf("get db[%d]: %w", dbIdx, err)
	}
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin tx: %w", tx.Error)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// 原 entry 是 credit (account.balance += credit_amount)；反向 = debit
	// (account.balance -= credit_amount)。具体方向由 IsAssetOrExpense 决定。
	var originalDelta int64
	if entry.CreditAmount > 0 {
		originalDelta = entry.CreditAmount
	} else {
		// 极少发生（debit credit entry 是平台手续费类账户），原始也是 debit；反向是 credit
		originalDelta = -entry.DebitAmount
	}

	// 锁账户 + 反向更新 balance
	acct, err := s.accountRepo.GetAccountForUpdate(ctx, tx, entry.AccountNo, dbIdx, tableIdx)
	if err != nil || acct == nil {
		tx.Rollback()
		return fmt.Errorf("lock account: %w", err)
	}
	balanceBefore := acct.Balance
	balanceAfter := balanceBefore - originalDelta // 反向：减去原增量
	accountTable := s.router.GetTableName("account", tableIdx)
	if err := tx.WithContext(ctx).Table(accountTable).
		Where("account_no = ?", entry.AccountNo).
		Updates(map[string]interface{}{
			"balance":           gorm.Expr("balance - ?", originalDelta),
			"available_balance": gorm.Expr("available_balance - ?", originalDelta),
			"version":           gorm.Expr("version + 1"),
		}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("apply reverse balance: %w", err)
	}

	// 写 reverse transaction record，与原 entry 配对（同 voucher_no）。
	reverseTxID := originalTxID + "-R" // 简单的命名约定：原 ID + "-R"
	desc := "REVERSE: " + entry.Description
	rev := &model.AccountTransaction{
		TransactionID:   reverseTxID,
		AccountNo:       entry.AccountNo,
		BusinessNo:      req.FreezeBusinessNo,
		BusinessType:    model.BusinessType(req.FreezeBusinessType),
		DebitAmount:     entry.CreditAmount,
		CreditAmount:    entry.DebitAmount,
		BalanceBefore:   balanceBefore,
		BalanceAfter:    balanceAfter,
		BookingType:     model.TransactionBookingTypeSync,
		Currency:        req.Currency,
		TransactionDate: transactionDate,
		TransactionTime: now,
		CutDate:         cutDate,
		Description:     &desc,
		Status:          model.TransactionStatusSuccess,
	}
	rev.ParentTransactionID = &voucherNo
	if err := s.transactionRepo.CreateTransaction(ctx, tx, rev, dbIdx, tableIdx); err != nil {
		// 幂等：reverseTxID = originalTxID+"-R" 唯一约束撞了说明已经反向过，
		// 直接 commit 当前 tx (空操作) 视为 idempotent 成功；balance UPDATE 在
		// 同一 tx 里会随之 commit。
		// **但** 这会导致 balance 第二次被减！必须 ROLLBACK，让账户余额不变，
		// 把"已反向"当成成功返回。
		if isDuplicateKeyError(err) {
			tx.Rollback()
			s.logger.Info("compensateCreditEntry: reverse tx already exists, idempotent skip",
				zap.String("accountNo", entry.AccountNo),
				zap.String("reverseTxID", reverseTxID))
			return nil
		}
		tx.Rollback()
		return fmt.Errorf("create reverse transaction: %w", err)
	}
	return tx.Commit().Error
}

// compensateFrozenDebit 反向冲账已 commit 的 Phase 1 frozen-debit。
//
// 原 Phase 1: balance -= A, frozen_balance -= A, freeze_order SUCCESS
// 反向：     balance += A, frozen_balance += A, freeze_order 状态留 SUCCESS
//            （回滚到 SUCCESS 之前比较复杂，且已经记了 reverse audit；让运维知道）
//
// 写一条 reverse transaction record（与原 frozen-debit 配对）。
func (s *freezeService) compensateFrozenDebit(
	ctx context.Context,
	req *UnfreezeAndDebitRequest,
	frozenAccountNo string,
	frozenAmount int64,
	dbIdx, tableIdx int,
	voucherNo string,
	transactionDate, cutDate string,
	now time.Time,
) error {
	db, err := s.dbManager.GetDB(dbIdx)
	if err != nil {
		return fmt.Errorf("get db[%d]: %w", dbIdx, err)
	}
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin tx: %w", tx.Error)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	acct, err := s.accountRepo.GetAccountForUpdate(ctx, tx, frozenAccountNo, dbIdx, tableIdx)
	if err != nil || acct == nil {
		tx.Rollback()
		return fmt.Errorf("lock frozen account: %w", err)
	}
	balanceBefore := acct.Balance
	balanceAfter := balanceBefore + frozenAmount

	accountTable := s.router.GetTableName("account", tableIdx)
	if err := tx.WithContext(ctx).Table(accountTable).
		Where("account_no = ?", frozenAccountNo).
		Updates(map[string]interface{}{
			"balance":        gorm.Expr("balance + ?", frozenAmount),
			"frozen_balance": gorm.Expr("frozen_balance + ?", frozenAmount),
			"version":        gorm.Expr("version + 1"),
		}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("apply reverse frozen-debit: %w", err)
	}

	// reverseTxID 必须 deterministic：worker 重试时，第一次写成功后第二次必须撞
	// transaction_id 唯一索引才能被 DUPLICATE KEY 拦截。如果用 generateFreezeTxID
	// 每次 fresh 一个新 ID，重试会无限累积反向 transaction → balance += A 累乘 →
	// 资损。固定 = voucherNo + "-FR" 保证幂等。注：voucherNo 已含分片前缀
	// (dbIdx + tableIdx)，"-FR" 后缀不破坏路由。
	reverseTxID := voucherNo + "-FR"
	desc := "REVERSE frozen-debit (compensating cancel)"
	rev := &model.AccountTransaction{
		TransactionID:   reverseTxID,
		AccountNo:       frozenAccountNo,
		BusinessNo:      req.FreezeBusinessNo,
		BusinessType:    model.BusinessType(req.FreezeBusinessType),
		DebitAmount:     0,
		CreditAmount:    frozenAmount,
		BalanceBefore:   balanceBefore,
		BalanceAfter:    balanceAfter,
		BookingType:     model.TransactionBookingTypeSync,
		Currency:        req.Currency,
		TransactionDate: transactionDate,
		TransactionTime: now,
		CutDate:         cutDate,
		Description:     &desc,
		Status:          model.TransactionStatusSuccess,
	}
	rev.ParentTransactionID = &voucherNo
	if err := s.transactionRepo.CreateTransaction(ctx, tx, rev, dbIdx, tableIdx); err != nil {
		if isDuplicateKeyError(err) {
			// 幂等：reverse 已存在；当前 tx rollback 不让 balance 重复 +A
			tx.Rollback()
			s.logger.Info("compensateFrozenDebit: reverse tx already exists, idempotent skip",
				zap.String("frozenAccountNo", frozenAccountNo),
				zap.String("reverseTxID", reverseTxID))
			return nil
		}
		tx.Rollback()
		return fmt.Errorf("create reverse frozen-debit transaction: %w", err)
	}

	// freeze_order 状态：之前是 SUCCESS（已 settled）。compensating 后留 SUCCESS
	// 但 audit trail 里有 reverse transaction，运维能看到 net 净额是 0。
	// 不强制改回 FROZEN，因为 freeze_order 状态机不允许 SUCCESS → FROZEN
	// 反向；要改的话需要 freeze_order 模型加 COMPENSATED 状态。
	// 这里仅 log 提示运维。
	s.logger.Warn("compensateFrozenDebit: reverse transaction recorded; freeze_order remains SUCCESS but net balance restored",
		zap.String("voucherNo", voucherNo),
		zap.String("freezeOrderNo", req.FreezeOrderNo),
		zap.String("reverseTxID", reverseTxID),
		zap.Int64("amount", frozenAmount))

	return tx.Commit().Error
}

// generateFreezeVoucherNo 按位编码生成冻结凭证号（与普通 voucher 共享 layout/idType=001）。
func (s *freezeService) generateFreezeVoucherNo(ctx context.Context, dbIdx, tableIdx int) (string, error) {
	_ = dbIdx // dbIdx 由 globalTblIdx 派生，不需要单独传
	seq, err := s.idGen.NextID(ctx, idgen.BizTagVoucher)
	if err != nil {
		return "", err
	}
	return shadow.EncodeIDStr(ctx, shadow.IDTypeVoucher, tableIdx, seq)
}

// generateFreezeTxID 按位编码生成冻结流水号（idType=003 区分于普通 transaction）。
func (s *freezeService) generateFreezeTxID(ctx context.Context, accountNo string) (string, error) {
	_, globalTblIdx := s.router.RouteByAccountNo(accountNo)
	seq, err := s.idGen.NextID(ctx, idgen.BizTagFreezeTx)
	if err != nil {
		return "", err
	}
	return shadow.EncodeIDStr(ctx, shadow.IDTypeFreezeTx, globalTblIdx, seq)
}
