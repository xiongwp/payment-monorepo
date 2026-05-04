package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	commonutil "github.com/accounting-system/internal/common"
	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/idgen"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/kafka"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/repository"
	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// HybridAccountingService 混合记账服务
// hybridShardConcurrency: HybridDoubleEntryBooking 内 per-shard goroutine
// 并发上限。单笔 booking 极端情况可能涉及 100 分片，不限流会把 DB 连接池
// 吃完。8 ≈ 物理 DB 数 / 留余量，避免大单 booking 把 池里的 conn 占满阻塞
// 其它 booking。可观测的上限指标见 metrics.HybridShardInflight。
const hybridShardConcurrency = 8

// 资金账户操作（用户、商户）：同步执行
// 会计分录记录：异步记录（通过消息队列）
type HybridAccountingService interface {
	// SyncUpdateBalance 同步更新账户余额（用户/商户账户）
	SyncUpdateBalance(ctx context.Context, req *BalanceUpdateRequest) (*BalanceUpdateResponse, error)

	// AsyncRecordEntry 异步记录会计分录（平台账户、中间账户等）
	AsyncRecordEntry(ctx context.Context, req *EntryRecordRequest) error

	// HybridDoubleEntryBooking 混合模式复式记账
	// 用户/商户账户：同步更新
	// 会计分录：异步记录
	HybridDoubleEntryBooking(ctx context.Context, req *HybridBookingRequest) (*HybridBookingResponse, error)
}

// BalanceUpdateRequest 余额更新请求
// Amount 单位：ISO 最小货币单位 × 100（参见 currency 包）
type BalanceUpdateRequest struct {
	AccountNo    string             `json:"account_no"`
	Amount       int64              `json:"amount"`
	IsDebit      bool               `json:"is_debit"` // true: 借方（增加资产）, false: 贷方（减少资产）
	BusinessNo   string             `json:"business_no"`
	BusinessType model.BusinessType `json:"business_type"`
	Description  string             `json:"description"`
}

// BalanceUpdateResponse 余额更新响应
type BalanceUpdateResponse struct {
	Success       bool   `json:"success"`
	TransactionID string `json:"transaction_id"`
	BalanceBefore int64  `json:"balance_before"`
	BalanceAfter  int64  `json:"balance_after"`
	ErrorMessage  string `json:"error_message,omitempty"`
}

// EntryRecordRequest 分录记录请求。
//
// RequestID 必填：作为 AsyncRecordEntry 的幂等键，下游 async_task 表通过
// uniq_request_id_type 约束防止 Kafka 重投或上游重试导致的重复入账。
type EntryRecordRequest struct {
	RequestID    string             `json:"request_id"`
	VoucherNo    string             `json:"voucher_no"`
	BusinessNo   string             `json:"business_no"`
	BusinessType model.BusinessType `json:"business_type"`
	Entries      []AccountingEntry  `json:"entries"`
	Currency     string             `json:"currency"`
	Description  string             `json:"description"`
}

// HybridBookingRequest 混合记账请求
type HybridBookingRequest struct {
	BusinessNo         string             `json:"business_no"`
	BusinessType       model.BusinessType `json:"business_type"`
	FundAccountEntries []AccountingEntry  `json:"fund_account_entries"` // 资金账户分录（同步）
	LedgerEntries      []AccountingEntry  `json:"ledger_entries"`       // 会计分录（异步）
	Currency           string             `json:"currency"`
	Description        string             `json:"description"`
}

// HybridBookingResponse 混合记账响应
type HybridBookingResponse struct {
	Success            bool     `json:"success"`
	VoucherNo          string   `json:"voucher_no"`
	FundTransactionIDs []string `json:"fund_transaction_ids"` // 资金账户交易ID
	LedgerTaskID       string   `json:"ledger_task_id"`       // 会计分录任务ID
	ErrorMessage       string   `json:"error_message,omitempty"`
}

type hybridAccountingService struct {
	accountRepo      repository.AccountRepository
	transactionRepo  repository.TransactionRepository
	dbManager        *database.Manager
	router           *sharding.Router
	kafkaProducer    *kafka.Producer
	asyncTaskService AsyncTaskService
	idGen            idgen.IDGenerator
	logger           *zap.Logger
}

// NewHybridAccountingService 创建混合记账服务
func NewHybridAccountingService(
	accountRepo repository.AccountRepository,
	transactionRepo repository.TransactionRepository,
	dbManager *database.Manager,
	router *sharding.Router,
	kafkaProducer *kafka.Producer,
	asyncTaskService AsyncTaskService,
	idGen idgen.IDGenerator,
	logger *zap.Logger,
) HybridAccountingService {
	return &hybridAccountingService{
		accountRepo:      accountRepo,
		transactionRepo:  transactionRepo,
		dbManager:        dbManager,
		router:           router,
		kafkaProducer:    kafkaProducer,
		asyncTaskService: asyncTaskService,
		idGen:            idGen,
		logger:           logger,
	}
}

// SyncUpdateBalance 同步更新账户余额
func (s *hybridAccountingService) SyncUpdateBalance(ctx context.Context, req *BalanceUpdateRequest) (*BalanceUpdateResponse, error) {
	s.logger.Info("sync update balance started",
		zap.String("accountNo", req.AccountNo),
		zap.Int64("amount", req.Amount),
		zap.Bool("isDebit", req.IsDebit),
	)

	// 1. 获取账户所在分片
	dbIndex, tableIndex := s.router.RouteByAccountNo(req.AccountNo)

	// 2. 获取数据库连接
	db, err := s.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("get db failed: %w", err)
	}

	// 3. 开启事务
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, fmt.Errorf("begin transaction failed: %w", tx.Error)
	}
	defer tx.Rollback()

	// 4. 查询账户并加锁
	account, err := s.accountRepo.GetAccountForUpdate(ctx, tx, req.AccountNo, dbIndex, tableIndex)
	if err != nil {
		return nil, fmt.Errorf("get account for update failed: %w", err)
	}
	if account == nil {
		return &BalanceUpdateResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("account not found: %s", req.AccountNo),
		}, nil
	}

	// 5. 检查账户类型（只允许用户和商户账户同步更新）
	if account.AccountType != model.AccountTypeUser && account.AccountType != model.AccountTypeMerchant {
		return &BalanceUpdateResponse{
			Success:      false,
			ErrorMessage: "only user and merchant accounts support sync update",
		}, nil
	}

	// Defense-in-depth：amount 必须 > 0。req 经 gRPC 进来已经被 validate 过一次，
	// 这里再守一道，避免恶意 negative / 零值滑进算术。
	if req.Amount <= 0 {
		return &BalanceUpdateResponse{
			Success:      false,
			ErrorMessage: "amount must be positive",
		}, nil
	}

	// 6. 计算新余额（safemath 防 int64 wrap-around）
	balanceBefore := account.Balance
	var balanceAfter int64
	var ok bool

	// 对于资产类账户：借方增加，贷方减少
	if account.AccountCategory == model.AccountCategoryAsset {
		if req.IsDebit {
			balanceAfter, ok = commonutil.AddInt64(balanceBefore, req.Amount)
		} else {
			balanceAfter, ok = commonutil.SubInt64(balanceBefore, req.Amount)
			// 检查余额是否足够
			if ok && !commonutil.IsAccountCanNegative(account.AccountType) && balanceAfter < 0 {
				return &BalanceUpdateResponse{
					Success:      false,
					ErrorMessage: "insufficient balance",
				}, nil
			}
		}
	} else {
		// 对于负债/权益类账户：借方减少，贷方增加
		if req.IsDebit {
			balanceAfter, ok = commonutil.SubInt64(balanceBefore, req.Amount)
		} else {
			balanceAfter, ok = commonutil.AddInt64(balanceBefore, req.Amount)
		}
	}
	if !ok {
		return &BalanceUpdateResponse{
			Success:      false,
			ErrorMessage: "balance arithmetic overflow",
		}, nil
	}

	// 7. 更新账户余额
	accountTableName := s.router.GetTableName("account", tableIndex)
	result := tx.WithContext(ctx).Table(accountTableName).
		Where("account_no = ? AND version = ?", req.AccountNo, account.Version).
		Updates(map[string]interface{}{
			"balance":           balanceAfter,
			"available_balance": balanceAfter - account.FrozenBalance,
			"version":           gorm.Expr("version + 1"),
		})

	if result.Error != nil {
		return nil, fmt.Errorf("update balance failed: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return &BalanceUpdateResponse{
			Success:      false,
			ErrorMessage: "update balance failed: version conflict",
		}, nil
	}

	// 8. 插入交易流水
	transactionID, err := s.generateTransactionID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate transaction id failed: %w", err)
	}
	now := time.Now()

	transaction := &model.AccountTransaction{
		TransactionID:   transactionID,
		AccountNo:       req.AccountNo,
		BusinessNo:      req.BusinessNo,
		BusinessType:    req.BusinessType,
		DebitAmount:     0,
		CreditAmount:    0,
		BalanceBefore:   balanceBefore,
		BalanceAfter:    balanceAfter,
		BookingType:     model.TransactionBookingTypeSync,
		Currency:        "PHP",
		TransactionDate: now.Format("2006-01-02"),
		TransactionTime: now,
		// cut_date 必须设：DB 列 NOT NULL，零值会写成 0000-00-00 让 trial
		// balance 按 cut_date 过滤时该 entry 错失，与同 voucher 其它 entry
		// 落不同日期，借贷不平。这里同 transaction_date（calendar day），
		// 整笔 hybrid 操作内的所有 entries 共享同一 cut_date。
		CutDate:         now.Format("2006-01-02"),
		Description:     &req.Description,
		Status:          model.TransactionStatusSuccess,
		RetryCount:      0,
	}

	if req.IsDebit {
		transaction.DebitAmount = req.Amount
	} else {
		transaction.CreditAmount = req.Amount
	}

	if err := s.transactionRepo.CreateTransaction(ctx, tx, transaction, dbIndex, tableIndex); err != nil {
		return nil, fmt.Errorf("create transaction failed: %w", err)
	}

	// 9. 提交事务
	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("commit transaction failed: %w", err)
	}

	s.logger.Info("sync update balance completed",
		zap.String("accountNo", req.AccountNo),
		zap.String("transactionID", transactionID),
		zap.String("balanceBefore", strconv.FormatInt(balanceBefore, 10)),
		zap.String("balanceAfter", strconv.FormatInt(balanceAfter, 10)),
	)

	return &BalanceUpdateResponse{
		Success:       true,
		TransactionID: transactionID,
		BalanceBefore: balanceBefore,
		BalanceAfter:  balanceAfter,
	}, nil
}

// AsyncRecordEntry 异步记录会计分录。
//
// 幂等：req.RequestID 必填。若同一 request_id 已有 async_task → CreateTask
// 命中 uniq 索引返回首次任务，不会重复入账；Kafka 端用 request_id 作 key
// 让重投落到同一分区，consumer 端再 dedup。
func (s *hybridAccountingService) AsyncRecordEntry(ctx context.Context, req *EntryRecordRequest) error {
	if req == nil || req.RequestID == "" {
		return fmt.Errorf("request_id is required for async record entry idempotency")
	}
	s.logger.Info("async record entry started",
		zap.String("requestID", req.RequestID),
		zap.String("voucherNo", req.VoucherNo),
		zap.String("businessNo", req.BusinessNo),
		zap.Int("entryCount", len(req.Entries)),
	)

	// 1. 创建异步任务（DB 端 uniq_request_id_type 约束兜底重复 RequestID）
	if err := s.asyncTaskService.CreateTask(
		ctx,
		"LEDGER_ENTRY",
		req.RequestID,
		req.BusinessNo,
		req,
		3, // 最大重试3次
	); err != nil {
		return fmt.Errorf("create async task failed: %w", err)
	}

	// 2. 发送到 Kafka。key 用 request_id：同 request_id 的重投保持同一分区，
	// consumer 端按 (request_id, task_type) 二次 dedup。
	if err := s.kafkaProducer.SendMessage(
		ctx,
		req.RequestID,
		"LEDGER_ENTRY",
		req,
	); err != nil {
		s.logger.Error("send kafka message failed", zap.Error(err))
		// Kafka 发送失败不影响任务创建，依靠定时任务重试
	}

	s.logger.Info("async record entry task created",
		zap.String("requestID", req.RequestID),
		zap.String("voucherNo", req.VoucherNo),
		zap.String("businessNo", req.BusinessNo),
	)

	return nil
}

// shardEntryGroup 同一分片下的资金账户分录批次
type shardEntryGroup struct {
	dbIndex    int
	tableIndex int
	entries    []AccountingEntry
}

// HybridDoubleEntryBooking 混合模式复式记账
//
// 性能优化（相对旧实现）：
//   - 按分片分组：同一分片的所有分录在单个 DB 事务内处理，减少 BEGIN/COMMIT 往返次数
//     从 N 次独立事务 → S 次（S = 涉及的分片数，S ≤ N）
//   - 跨分片并行：不同分片的事务并发执行，整体延迟 ≈ 最慢单分片，而非所有分片之和
//   - 锁序固定：同一分片内按 account_no 升序加锁，消除死锁风险
func (s *hybridAccountingService) HybridDoubleEntryBooking(ctx context.Context, req *HybridBookingRequest) (*HybridBookingResponse, error) {
	s.logger.Info("hybrid double entry booking started",
		zap.String("businessNo", req.BusinessNo),
		zap.Int("fundEntries", len(req.FundAccountEntries)),
		zap.Int("ledgerEntries", len(req.LedgerEntries)),
	)

	voucherNo, err := s.generateVoucherNo(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate voucher no failed: %w", err)
	}

	// 1. 按分片分组，组内按 account_no 升序排列（固定加锁顺序，防死锁）
	groupMap := make(map[[2]int][]int) // [dbIndex, tableIndex] → entry indices
	for i, entry := range req.FundAccountEntries {
		dbIdx, tableIdx := s.router.RouteByAccountNo(entry.AccountNo)
		key := [2]int{dbIdx, tableIdx}
		groupMap[key] = append(groupMap[key], i)
	}
	groups := make([]shardEntryGroup, 0, len(groupMap))
	for key, idxs := range groupMap {
		// 按 account_no 升序排序以固定锁顺序
		sort.Slice(idxs, func(a, b int) bool {
			return req.FundAccountEntries[idxs[a]].AccountNo < req.FundAccountEntries[idxs[b]].AccountNo
		})
		grpEntries := make([]AccountingEntry, len(idxs))
		for j, i := range idxs {
			grpEntries[j] = req.FundAccountEntries[i]
		}
		groups = append(groups, shardEntryGroup{
			dbIndex:    key[0],
			tableIndex: key[1],
			entries:    grpEntries,
		})
	}

	// 2. 并发执行每个分片的批量余额更新。
	//
	// 用 chan-based semaphore 限并发：极端跨租户 booking 可能涉及全部 100 分片，
	// 不限流 = 100 goroutine 同时跑，把 DB 连接池吃完（每库默认 ~50 conn）+
	// 上下文切换雪崩。固定 8 路并行：和单库连接池 + 物理 DB 数（10）大致匹
	// 配，不会浪费 CPU 也不会爆池。
	type shardResult struct {
		txIDs []string
		err   string // non-empty = failure
	}
	shardResults := make([]shardResult, len(groups))
	var wg sync.WaitGroup
	sem := make(chan struct{}, hybridShardConcurrency)
	for i, grp := range groups {
		i, grp := i, grp
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				shardResults[i] = shardResult{err: ctx.Err().Error()}
				return
			}
			ids, errMsg := s.syncUpdateBalanceBatch(ctx, grp, req)
			shardResults[i] = shardResult{txIDs: ids, err: errMsg}
		}()
	}
	wg.Wait()

	// 3. 收集结果
	var fundTransactionIDs []string
	for _, res := range shardResults {
		if res.err != "" {
			s.logger.Error("hybrid: shard balance update failed", zap.String("error", res.err))
			return &HybridBookingResponse{Success: false, ErrorMessage: res.err}, nil
		}
		fundTransactionIDs = append(fundTransactionIDs, res.txIDs...)
	}

	// 4. 异步记录会计分录
	var ledgerTaskID string
	if len(req.LedgerEntries) > 0 {
		taskID, idErr := s.idGen.NextIDStr(ctx, idgen.BizTagAsyncTask)
		if idErr == nil {
			ledgerTaskID = taskID
		} else {
			s.logger.Warn("hybrid: generate ledger task id failed", zap.Error(idErr))
		}

		if err := s.AsyncRecordEntry(ctx, &EntryRecordRequest{
			VoucherNo:    voucherNo,
			BusinessNo:   req.BusinessNo,
			BusinessType: req.BusinessType,
			Entries:      req.LedgerEntries,
			Currency:     req.Currency,
			Description:  req.Description,
		}); err != nil {
			s.logger.Error("async record entry failed", zap.Error(err))
		}
	}

	s.logger.Info("hybrid double entry booking completed",
		zap.String("businessNo", req.BusinessNo),
		zap.String("voucherNo", voucherNo),
		zap.Strings("fundTransactionIDs", fundTransactionIDs),
		zap.String("ledgerTaskID", ledgerTaskID),
	)

	return &HybridBookingResponse{
		Success:            true,
		VoucherNo:          voucherNo,
		FundTransactionIDs: fundTransactionIDs,
		LedgerTaskID:       ledgerTaskID,
	}, nil
}

// syncUpdateBalanceBatch 在单个 DB 事务中处理同一分片的所有资金账户分录。
//
// 调用方须保证 grp.entries 已按 account_no 升序排列（固定锁顺序，防止死锁）。
// 返回 (txIDs, errMsg)；errMsg 非空表示失败。
func (s *hybridAccountingService) syncUpdateBalanceBatch(
	ctx context.Context,
	grp shardEntryGroup,
	req *HybridBookingRequest,
) (txIDs []string, errMsg string) {
	db, err := s.dbManager.GetDB(grp.dbIndex)
	if err != nil {
		return nil, fmt.Sprintf("get db[%d]: %v", grp.dbIndex, err)
	}

	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, fmt.Sprintf("begin tx: %v", tx.Error)
	}
	defer func() {
		if errMsg != "" {
			tx.Rollback() //nolint:errcheck
		}
	}()

	now := time.Now()
	txIDs = make([]string, 0, len(grp.entries))

	for _, entry := range grp.entries {
		// SELECT ... FOR UPDATE（同一分片，在同一个 tx 中）
		account, err := s.accountRepo.GetAccountForUpdate(ctx, tx, entry.AccountNo, grp.dbIndex, grp.tableIndex)
		if err != nil {
			return nil, fmt.Sprintf("lock account %s: %v", entry.AccountNo, err)
		}
		if account == nil {
			return nil, fmt.Sprintf("account not found: %s", entry.AccountNo)
		}
		if account.AccountType != model.AccountTypeUser && account.AccountType != model.AccountTypeMerchant {
			return nil, fmt.Sprintf("account %s type %d not allowed for sync update", entry.AccountNo, account.AccountType)
		}

		// 计算方向 & 金额
		var isDebit bool
		var amount int64
		if entry.DebitAmount != 0 {
			isDebit = true
			amount = entry.DebitAmount
		} else {
			amount = entry.CreditAmount
		}

		// Defense-in-depth：entry amount 必须 > 0。
		if amount <= 0 {
			return nil, fmt.Sprintf("entry amount must be positive for account %s", entry.AccountNo)
		}

		// 计算新余额（safemath 防 int64 wrap-around；与 SyncUpdateBalance 相同逻辑）
		balanceBefore := account.Balance
		var balanceAfter int64
		var ok bool
		if account.AccountCategory == model.AccountCategoryAsset {
			if isDebit {
				balanceAfter, ok = commonutil.AddInt64(balanceBefore, amount)
			} else {
				balanceAfter, ok = commonutil.SubInt64(balanceBefore, amount)
				if ok && !commonutil.IsAccountCanNegative(account.AccountType) && balanceAfter < 0 {
					return nil, fmt.Sprintf("insufficient balance for account %s", entry.AccountNo)
				}
			}
		} else {
			if isDebit {
				balanceAfter, ok = commonutil.SubInt64(balanceBefore, amount)
			} else {
				balanceAfter, ok = commonutil.AddInt64(balanceBefore, amount)
			}
		}
		if !ok {
			return nil, fmt.Sprintf("balance arithmetic overflow for account %s", entry.AccountNo)
		}

		// UPDATE account（乐观锁 + version 自增）
		accountTable := s.router.GetTableName("account", grp.tableIndex)
		res := tx.Table(accountTable).
			Where("account_no = ? AND version = ?", entry.AccountNo, account.Version).
			Updates(map[string]interface{}{
				"balance":           balanceAfter,
				"available_balance": balanceAfter - account.FrozenBalance,
				"version":           gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return nil, fmt.Sprintf("update balance for %s: %v", entry.AccountNo, res.Error)
		}
		if res.RowsAffected == 0 {
			return nil, fmt.Sprintf("version conflict for account %s", entry.AccountNo)
		}

		// 生成 transaction_id
		txIDraw, err := s.idGen.NextIDStr(ctx, idgen.BizTagTransaction)
		if err != nil {
			return nil, fmt.Sprintf("generate transaction id: %v", err)
		}
		txID := fmt.Sprintf("T%s", txIDraw)

		// INSERT account_transaction（幂等：transaction_id 唯一索引）
		desc := entry.Description
		var debitAmt, creditAmt int64
		if isDebit {
			debitAmt = amount
		} else {
			creditAmt = amount
		}
		transaction := &model.AccountTransaction{
			TransactionID:   txID,
			AccountNo:       entry.AccountNo,
			BusinessNo:      req.BusinessNo,
			BusinessType:    req.BusinessType,
			DebitAmount:     debitAmt,
			CreditAmount:    creditAmt,
			BalanceBefore:   balanceBefore,
			BalanceAfter:    balanceAfter,
			BookingType:     model.TransactionBookingTypeSync,
			Currency:        "PHP",
			TransactionDate: now.Format("2006-01-02"),
			TransactionTime: now,
			// cut_date 必须设；同 hybrid batch 所有 entry 共享同一 cutDate。
			CutDate:         now.Format("2006-01-02"),
			Description:     &desc,
			Status:          model.TransactionStatusSuccess,
		}
		if err := s.transactionRepo.CreateTransaction(ctx, tx, transaction, grp.dbIndex, grp.tableIndex); err != nil {
			return nil, fmt.Sprintf("create transaction for %s: %v", entry.AccountNo, err)
		}
		txIDs = append(txIDs, txID)
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Sprintf("commit: %v", err)
	}
	return txIDs, ""
}

// generateVoucherNo 按位编码生成凭证号（与主路径共享 idType=001）。
// hybrid 路径不在某个固定 shard 上，globalTbl 用 0 占位。
func (s *hybridAccountingService) generateVoucherNo(ctx context.Context) (string, error) {
	seq, err := s.idGen.NextID(ctx, idgen.BizTagVoucher)
	if err != nil {
		return "", fmt.Errorf("hybrid: generate voucher no: %w", err)
	}
	return shadow.EncodeIDStr(ctx, shadow.IDTypeVoucher, 0, seq)
}

// generateTransactionID 按位编码生成交易流水号（idType=002）。
func (s *hybridAccountingService) generateTransactionID(ctx context.Context) (string, error) {
	seq, err := s.idGen.NextID(ctx, idgen.BizTagTransaction)
	if err != nil {
		return "", fmt.Errorf("hybrid: generate transaction id: %w", err)
	}
	return shadow.EncodeIDStr(ctx, shadow.IDTypeTransaction, 0, seq)
}
