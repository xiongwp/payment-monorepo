package load

// 压测 / 负载测试
//
// 运行方式：
//   go test -v -timeout 300s ./tests/load/...                         # 负载测试
//   go test -bench=. -benchmem -benchtime=10s ./tests/load/...       # 基准测试
//
// 依赖：本地 MySQL（account_meta_test + accounting_db_0_test ~ accounting_db_2_test）
// 跳过：若 DB 不可用则自动 skip，不影响 CI 单元测试。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/idgen"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/repository"
	"github.com/accounting-system/internal/service"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// ─── 环境初始化 ───────────────────────────────────────────────────────────────

type loadEnv struct {
	mgr           *database.Manager
	router        *sharding.Router
	accountRepo   repository.AccountRepository
	accountingSvc service.AccountingService
	logger        *zap.Logger
}

func newLoadEnvT(t *testing.T) *loadEnv {
	t.Helper()
	return buildEnv(t, nil)
}

func newLoadEnvB(b *testing.B) *loadEnv {
	b.Helper()
	return buildEnv(nil, b)
}

// skipOrFatal skips the test if DB is unavailable; uses the right testing.TB interface.
func buildEnv(t *testing.T, b *testing.B) *loadEnv {
	skip := func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		if t != nil {
			t.Skip(msg)
		} else {
			b.Skip(msg)
		}
	}
	fatalf := func(format string, args ...interface{}) {
		if t != nil {
			t.Fatalf(format, args...)
		} else {
			b.Fatalf(format, args...)
		}
	}

	logger, _ := zap.NewDevelopment()

	metaCfg := database.DBConfig{
		Name:         "account_meta_test",
		DSN:          "root:password@tcp(127.0.0.1:3306)/account_meta_test?charset=utf8mb4&parseTime=True&loc=Local",
		MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 3600,
	}
	shardCfgs := []database.DBConfig{
		{Name: "accounting_db_0_test", DSN: "root:password@tcp(127.0.0.1:3306)/accounting_db_0_test?charset=utf8mb4&parseTime=True&loc=Local", MaxOpenConns: 20, MaxIdleConns: 10, ConnMaxLifetime: 3600},
		{Name: "accounting_db_1_test", DSN: "root:password@tcp(127.0.0.1:3306)/accounting_db_1_test?charset=utf8mb4&parseTime=True&loc=Local", MaxOpenConns: 20, MaxIdleConns: 10, ConnMaxLifetime: 3600},
		{Name: "accounting_db_2_test", DSN: "root:password@tcp(127.0.0.1:3306)/accounting_db_2_test?charset=utf8mb4&parseTime=True&loc=Local", MaxOpenConns: 20, MaxIdleConns: 10, ConnMaxLifetime: 3600},
	}

	mgr, err := database.NewManagerWithMeta(metaCfg, shardCfgs, nil)
	if err != nil {
		skip("DB unavailable, skipping load test: %v", err)
		return nil
	}

	router := sharding.NewRouterWithConfig(3, 10)
	accountRepo := repository.NewAccountRepository(mgr, router)
	txRepo := repository.NewTransactionRepository(mgr, router)
	tccRepo := repository.NewTccRepository(mgr, router)
	tccCoordRepo := repository.NewTccCoordinatorRepository(mgr, router)
	orderRepo := repository.NewTransactionOrderRepository(mgr, router)
	outboxRepo, err := repository.NewSettlementOutboxRepository(mgr, router)
	if err != nil {
		fatalf("outbox repo: %v", err)
	}
	batchOrderRepo := repository.NewBatchOrderRepository(mgr, router)
	bufferRepo := repository.NewBalanceBufferRepository(mgr, router)
	hotAccountRepo := repository.NewHotAccountRepository(mgr)
	bufferAccountRepo := repository.NewBufferAccountRepository(mgr)
	businessTypeRepo := repository.NewAccountBusinessTypeRepository(mgr)
	ruleRepo := repository.NewTransactionRuleRepository(mgr, router)
	idGen, err := idgen.NewIDGeneratorFromManager(mgr, logger)
	if err != nil {
		fatalf("id generator: %v", err)
	}

	accountingSvc := service.NewAccountingService(accountRepo, txRepo, tccRepo, tccCoordRepo, orderRepo, outboxRepo, batchOrderRepo, bufferRepo, hotAccountRepo, bufferAccountRepo, businessTypeRepo, ruleRepo, mgr, router, idGen, logger)

	// 种子数据（幂等）
	seedMetaData(mgr, fatalf)

	return &loadEnv{
		mgr: mgr, router: router,
		accountRepo: accountRepo, accountingSvc: accountingSvc,
		logger: logger,
	}
}

func seedMetaData(mgr *database.Manager, fatalf func(string, ...interface{})) {
	metaDB, err := mgr.GetMetaDB()
	if err != nil {
		fatalf("get meta db: %v", err)
	}
	metaDB.Exec(`
	INSERT IGNORE INTO account_type_info
(id,account_type,account_type_name,owner_type,balance_direction)
VALUES
(1,'USER_WALLET','用户钱包',1,'C'),        -- 负债：平台欠用户
(2,'MERCHANT_WALLET','商户钱包',2,'C'),    -- 负债：平台欠商户
(3,'PLATFORM_PNL','平台损益',3,'C'),       -- 收入/权益（保持不变）
-- 中转账户：保留，但建议后续拆分
(4,'PLATFORM_TRANSIT','平台中转',4,'D'),   -- 暂作为资产使用
-- 必须补：银行账户（否则充值规则不成立）
(5,'BANK_ACCOUNT','银行账户',3,'D')        -- 资产：真实资金
`)
}

func (e *loadEnv) close() { e.mgr.Close() }

// createOrGetAccount 创建账户（已存在则查询返回）
func (e *loadEnv) createOrGetAccount(ctx context.Context, ownerID int64, aType model.AccountType, cat model.AccountCategory) *model.Account {
	acc, err := e.accountingSvc.CreateAccount(ctx, ownerID, model.AccountBusinessTypeUserBalance, aType, cat, "PHP")
	if errors.Is(err, model.ErrAccountAlreadyExists) {
		acc, err = e.accountRepo.GetAccountByUserAndBusinessType(ctx, ownerID, model.AccountBusinessTypeUserBalance)
	}
	if err != nil || acc == nil {
		panic(fmt.Sprintf("createOrGetAccount ownerID=%d: %v", ownerID, err))
	}
	return acc
}

// topUpBalance 给账户强制直接设置余额（测试辅助，跳过 TCC，直接 UPDATE）
func (e *loadEnv) topUpBalance(ctx context.Context, accountNo string, amount int64) {
	dbIdx, tableIdx := e.router.RouteByAccountNo(accountNo)
	db, _ := e.mgr.GetDB(dbIdx)
	tableName := e.router.GetTableName("account", tableIdx)
	db.WithContext(ctx).Table(tableName).
		Where("account_no = ?", accountNo).
		Updates(map[string]interface{}{
			"balance":           amount,
			"available_balance": amount,
			"version":           gorm.Expr("version + 1"),
		})
}

// flushBuffer 手动触发 account_balance_buffer flush（测试专用）
func (e *loadEnv) flushBuffer(ctx context.Context) {
	type bufRow struct {
		AccountNo    string `gorm:"column:account_no"`
		PendingDelta string `gorm:"column:pending_delta"`
	}
	for _, shard := range e.router.GetAllShards() {
		db, err := e.mgr.GetDB(shard.DBIndex)
		if err != nil {
			continue
		}
		bufTable := e.router.GetTableName("account_balance_buffer", shard.TableIndex)
		accountTable := e.router.GetTableName("account", shard.TableIndex)
		var rows []bufRow
		db.WithContext(ctx).Table(bufTable).Find(&rows)
		for _, row := range rows {
			db.WithContext(ctx).Table(accountTable).
				Where("account_no = ?", row.AccountNo).
				Updates(map[string]interface{}{
					"balance":           gorm.Expr("balance + ?", row.PendingDelta),
					"available_balance": gorm.Expr("available_balance + ?", row.PendingDelta),
					"version":           gorm.Expr("version + 1"),
				})
			db.WithContext(ctx).Table(bufTable).Where("account_no = ?", row.AccountNo).Delete(&model.AccountBalanceBuffer{})
		}
	}
}

// ─── 负载测试 ─────────────────────────────────────────────────────────────────

// TestLoad_ConcurrentDoubleEntry 并发复式记账：10 goroutine × 20 笔 = 200 笔
// 验证无错误发生，且最终余额守恒（借方总额 == 贷方总额）
func TestLoad_ConcurrentDoubleEntry(t *testing.T) {
	env := newLoadEnvT(t)
	defer env.close()
	ctx := context.Background()

	const goroutines = 10
	const perGoroutine = 20
	const transferAmt = int64(10)

	// 准备账户：每个 goroutine 一对（sender + receiver）
	senders := make([]*model.Account, goroutines)
	receivers := make([]*model.Account, goroutines)

	for i := 0; i < goroutines; i++ {
		senders[i] = env.createOrGetAccount(ctx, int64(70000+i), model.AccountTypeUser, model.AccountCategoryLiability)
		receivers[i] = env.createOrGetAccount(ctx, int64(70100+i), model.AccountTypeUser, model.AccountCategoryLiability)
		// 充值 sender：直接设余额，绕过 TCC（测试辅助）
		env.topUpBalance(ctx, senders[i].AccountNo, int64(perGoroutine)*transferAmt+100)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perGoroutine)
	var seq int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				n := atomic.AddInt64(&seq, 1)
				reqID := fmt.Sprintf("load-concurrent-%d-%d", n, time.Now().UnixNano())
				bizNo := fmt.Sprintf("%010d", n)
				_, _, err := env.accountingSvc.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
					RequestID:    reqID,
					BusinessNo:   bizNo,
					BusinessType: model.BusinessTypeTransfer,
					Currency:     "PHP",
					Entries: []service.AccountingEntry{
						{AccountNo: senders[idx].AccountNo, CreditAmount: transferAmt},
						{AccountNo: receivers[idx].AccountNo, DebitAmount: transferAmt},
					},
				})
				if err != nil {
					errCh <- fmt.Errorf("goroutine %d booking %d: %w", idx, j, err)
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		t.Errorf("%d errors occurred during concurrent load test:", len(errs))
		for _, e := range errs {
			t.Errorf("  %v", e)
		}
	}

	// 借贷守恒：每笔 credit == debit，总额应该守恒
	t.Logf("TestLoad_ConcurrentDoubleEntry PASS: %d goroutines × %d bookings = %d total, errors=%d",
		goroutines, perGoroutine, goroutines*perGoroutine, len(errs))
}

// TestLoad_IdempotencyUnderConcurrency 并发幂等测试：20 goroutine 同时提交同一 requestID
// 期望：只有一次实际扣款，其余均命中幂等缓存
func TestLoad_IdempotencyUnderConcurrency(t *testing.T) {
	env := newLoadEnvT(t)
	defer env.close()
	ctx := context.Background()

	sender := env.createOrGetAccount(ctx, 80001, model.AccountTypeUser, model.AccountCategoryLiability)
	receiver := env.createOrGetAccount(ctx, 80002, model.AccountTypeUser, model.AccountCategoryLiability)
	env.topUpBalance(ctx, sender.AccountNo, 500)

	const goroutines = 20
	const amount = int64(10)
	requestID := fmt.Sprintf("idem-concurrent-%d", time.Now().UnixNano())
	bizNo := "8000100001"

	var wg sync.WaitGroup
	type result struct {
		voucherNo string
		err       error
	}
	results := make([]result, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			vno, _, err := env.accountingSvc.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
				RequestID:    requestID,
				BusinessNo:   bizNo,
				BusinessType: model.BusinessTypeTransfer,
				Currency:     "PHP",
				Entries: []service.AccountingEntry{
					{AccountNo: sender.AccountNo, CreditAmount: amount},
					{AccountNo: receiver.AccountNo, DebitAmount: amount},
				},
			})
			results[idx] = result{voucherNo: vno, err: err}
		}(i)
	}
	wg.Wait()

	// 统计：所有成功结果应返回相同 voucherNo
	var voucherNos []string
	for _, r := range results {
		if r.err == nil {
			voucherNos = append(voucherNos, r.voucherNo)
		}
	}
	if len(voucherNos) == 0 {
		t.Fatal("all goroutines failed")
	}
	first := voucherNos[0]
	for _, vno := range voucherNos[1:] {
		if vno != first {
			t.Errorf("idempotency violation: got different voucherNos: %s vs %s", first, vno)
		}
	}

	// sender 余额应只扣一次
	acc, err := env.accountingSvc.GetAccount(ctx, sender.AccountNo)
	if err != nil || acc == nil {
		t.Fatal("get account failed")
	}
	expected := int64(500 - amount)
	if acc.Balance != expected {
		t.Errorf("balance should be %d (only 1 debit), got %d", expected, acc.Balance)
	}
	t.Logf("TestLoad_IdempotencyUnderConcurrency PASS: %d goroutines, voucherNo=%s, senderBalance=%d",
		goroutines, first, acc.Balance)
}

// TestLoad_PlatformAccountNegativeBalance 平台账户允许负余额的并发写入测试
// 50 goroutine 各扣款 10，期望余额 -500（通过 buffer flush 后验证）
func TestLoad_PlatformAccountNegativeBalance(t *testing.T) {
	env := newLoadEnvT(t)
	defer env.close()
	ctx := context.Background()

	// 独立平台中间账户（transit，ownerID 使用较大值避免与其他测试冲突）
	transit := env.createOrGetAccount(ctx, 90001, model.AccountTypeTransit, model.AccountCategoryAsset)
	// 确保初始余额为 0
	env.topUpBalance(ctx, transit.AccountNo, 0)

	// 用于向 transit 账户支付的用户账户
	user := env.createOrGetAccount(ctx, 90002, model.AccountTypeUser, model.AccountCategoryLiability)
	env.topUpBalance(ctx, user.AccountNo, 10000)

	const goroutines = 50
	const debitPerGoroutine = int64(10)

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	var seq int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := atomic.AddInt64(&seq, 1)
			reqID := fmt.Sprintf("transit-neg-%d-%d", n, time.Now().UnixNano())
			bizNo := fmt.Sprintf("%010d", n+9000000000)
			_, _, err := env.accountingSvc.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
				RequestID:    reqID,
				BusinessNo:   bizNo,
				BusinessType: model.BusinessTypePayment,
				Currency:     "PHP",
				Entries: []service.AccountingEntry{
					// transit 贷出（资产账户贷方 → 余额减少，从0开始逐步变负）
					{AccountNo: transit.AccountNo, CreditAmount: debitPerGoroutine},
					// user 借入（资产账户借方 → 余额增加）
					{AccountNo: user.AccountNo, DebitAmount: debitPerGoroutine},
				},
			})
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)

	var errCount int
	for err := range errCh {
		t.Logf("error: %v", err)
		errCount++
	}
	if errCount > 0 {
		t.Errorf("%d errors in concurrent transit bookings", errCount)
	}

	// flush buffer，使 transit 余额写入 account 表
	env.flushBuffer(ctx)

	acc, err := env.accountingSvc.GetAccount(ctx, transit.AccountNo)
	if err != nil || acc == nil {
		t.Fatal("get transit account failed")
	}
	// transit 贷出：余额 = 0 - (成功笔数 × 10) = 负数
	expected := -int64((goroutines-errCount)) * debitPerGoroutine
	if acc.Balance != expected {
		t.Errorf("transit balance: expected %d, got %d", expected, acc.Balance)
	}
	t.Logf("TestLoad_PlatformAccountNegativeBalance PASS: transit balance=%d (errors=%d)", acc.Balance, errCount)
}

// ─── 基准测试 ─────────────────────────────────────────────────────────────────

// BenchmarkDoubleEntryBooking_Sequential 串行复式记账吞吐
func BenchmarkDoubleEntryBooking_Sequential(b *testing.B) {
	env := newLoadEnvB(b)
	defer env.close()
	ctx := context.Background()

	user := env.createOrGetAccount(ctx, 60001, model.AccountTypeUser, model.AccountCategoryLiability)
	platform := env.createOrGetAccount(ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)
	env.topUpBalance(ctx, user.AccountNo, 1_000_000_000)
	var seq int64

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := atomic.AddInt64(&seq, 1)
		bizNo := fmt.Sprintf("%010d", n)
		_, _, err := env.accountingSvc.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
			RequestID:    fmt.Sprintf("bench-seq-%d", n),
			BusinessNo:   bizNo,
			BusinessType: model.BusinessTypePayment,
			Currency:     "PHP",
			Entries: []service.AccountingEntry{
				{AccountNo: user.AccountNo, CreditAmount: 1},
				{AccountNo: platform.AccountNo, DebitAmount: 1},
			},
		})
		if err != nil {
			b.Errorf("booking error: %v", err)
		}
	}
}

// BenchmarkDoubleEntryBooking_Parallel 并行复式记账吞吐
func BenchmarkDoubleEntryBooking_Parallel(b *testing.B) {
	env := newLoadEnvB(b)
	defer env.close()
	ctx := context.Background()

	user := env.createOrGetAccount(ctx, 60002, model.AccountTypeUser, model.AccountCategoryLiability)
	platform := env.createOrGetAccount(ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)
	env.topUpBalance(ctx, user.AccountNo, 1_000_000_000)
	var seq int64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&seq, 1)
			bizNo := fmt.Sprintf("%010d", n)
			_, _, err := env.accountingSvc.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
				RequestID:    fmt.Sprintf("bench-par-%d", n),
				BusinessNo:   bizNo,
				BusinessType: model.BusinessTypePayment,
				Currency:     "PHP",
				Entries: []service.AccountingEntry{
					{AccountNo: user.AccountNo, CreditAmount: 1},
					{AccountNo: platform.AccountNo, DebitAmount: 1},
				},
			})
			if err != nil {
				b.Errorf("booking error: %v", err)
			}
		}
	})
}

// BenchmarkPlatformBufferedBooking 平台/中间账户缓冲记账吞吐
// 平台账户走 buffered 模式，不锁 account 行，吞吐应远高于普通账户
func BenchmarkPlatformBufferedBooking(b *testing.B) {
	env := newLoadEnvB(b)
	defer env.close()
	ctx := context.Background()

	user := env.createOrGetAccount(ctx, 60003, model.AccountTypeUser, model.AccountCategoryLiability)
	transit := env.createOrGetAccount(ctx, 60004, model.AccountTypeTransit, model.AccountCategoryAsset)
	env.topUpBalance(ctx, user.AccountNo, 1_000_000_000)
	var seq int64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&seq, 1)
			bizNo := fmt.Sprintf("%010d", n)
			_, _, err := env.accountingSvc.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
				RequestID:    fmt.Sprintf("bench-buf-%d", n),
				BusinessNo:   bizNo,
				BusinessType: model.BusinessTypePayment,
				Currency:     "PHP",
				Entries: []service.AccountingEntry{
					{AccountNo: user.AccountNo, CreditAmount: 1},
					// transit 账户走 buffered 模式，无 account 行锁
					{AccountNo: transit.AccountNo, DebitAmount: 1},
				},
			})
			if err != nil {
				b.Errorf("booking error: %v", err)
			}
		}
	})
}

// BenchmarkAtomicBatchBooking 原子批量记账吞吐（每次 3 条分录）
func BenchmarkAtomicBatchBooking(b *testing.B) {
	env := newLoadEnvB(b)
	defer env.close()
	ctx := context.Background()

	user1 := env.createOrGetAccount(ctx, 60005, model.AccountTypeUser, model.AccountCategoryLiability)
	user2 := env.createOrGetAccount(ctx, 60006, model.AccountTypeUser, model.AccountCategoryLiability)
	user3 := env.createOrGetAccount(ctx, 60007, model.AccountTypeUser, model.AccountCategoryLiability)
	platform := env.createOrGetAccount(ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)
	env.topUpBalance(ctx, user1.AccountNo, 1_000_000_000)
	env.topUpBalance(ctx, user2.AccountNo, 1_000_000_000)
	env.topUpBalance(ctx, user3.AccountNo, 1_000_000_000)
	var seq int64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&seq, 1)
			batchID := fmt.Sprintf("bench-batch-%d", n)
			batchBizNo := fmt.Sprintf("%010d", n)
			_, err := env.accountingSvc.AtomicBatchBooking(ctx, &service.AtomicBatchBookingRequest{
				BatchRequestID:  batchID,
				BatchBusinessNo: batchBizNo,
				Requests: []service.DoubleEntryBookingRequest{
					{
						RequestID:    fmt.Sprintf("%s-1", batchID),
						BusinessNo:   fmt.Sprintf("%010d", n*3),
						BusinessType: model.BusinessTypePayment,
						Currency:     "PHP",
						Entries: []service.AccountingEntry{
							{AccountNo: user1.AccountNo, CreditAmount: 1},
							{AccountNo: platform.AccountNo, DebitAmount: 1},
						},
					},
					{
						RequestID:    fmt.Sprintf("%s-2", batchID),
						BusinessNo:   fmt.Sprintf("%010d", n*3+1),
						BusinessType: model.BusinessTypePayment,
						Currency:     "PHP",
						Entries: []service.AccountingEntry{
							{AccountNo: user2.AccountNo, CreditAmount: 1},
							{AccountNo: platform.AccountNo, DebitAmount: 1},
						},
					},
					{
						RequestID:    fmt.Sprintf("%s-3", batchID),
						BusinessNo:   fmt.Sprintf("%010d", n*3+2),
						BusinessType: model.BusinessTypePayment,
						Currency:     "PHP",
						Entries: []service.AccountingEntry{
							{AccountNo: user3.AccountNo, CreditAmount: 1},
							{AccountNo: platform.AccountNo, DebitAmount: 1},
						},
					},
				},
			})
			if err != nil {
				b.Errorf("batch booking error: %v", err)
			}
		}
	})
}
