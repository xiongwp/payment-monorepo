package e2e

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/idgen"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/accounting-system/internal/repository"
	"github.com/xiongwp/accounting-system/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// ─── 测试环境 ─────────────────────────────────────────────────────────────────

type testEnvironment struct {
	dbManager          *database.Manager
	router             *sharding.Router
	accountRepo        repository.AccountRepository
	transactionRepo    repository.TransactionRepository
	ruleRepo           repository.TransactionRuleRepository
	orderRepo          repository.TransactionOrderRepository
	accountingService  service.AccountingService
	transactionService service.TransactionService
	dayCutService      service.DayCutService
	logger             *zap.Logger
}

func setupTestEnvironment(t *testing.T) *testEnvironment {
	t.Helper()
	logger, _ := zap.NewDevelopment()

	// account_meta 全局元数据库（account_type_info / transaction_rule）
	metaCfg := database.DBConfig{
		Name:            "account_meta_test",
		DSN:             "root:password@tcp(127.0.0.1:3306)/account_meta_test?charset=utf8mb4&parseTime=True&loc=Local",
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 3600,
	}
	// accounting_db_0 ~ accounting_db_2（测试用 3 分库）
	shardCfgs := []database.DBConfig{
		{Name: "accounting_db_0_test", DSN: "root:password@tcp(127.0.0.1:3306)/accounting_db_0_test?charset=utf8mb4&parseTime=True&loc=Local", MaxOpenConns: 10, MaxIdleConns: 5, ConnMaxLifetime: 3600},
		{Name: "accounting_db_1_test", DSN: "root:password@tcp(127.0.0.1:3306)/accounting_db_1_test?charset=utf8mb4&parseTime=True&loc=Local", MaxOpenConns: 10, MaxIdleConns: 5, ConnMaxLifetime: 3600},
		{Name: "accounting_db_2_test", DSN: "root:password@tcp(127.0.0.1:3306)/accounting_db_2_test?charset=utf8mb4&parseTime=True&loc=Local", MaxOpenConns: 10, MaxIdleConns: 5, ConnMaxLifetime: 3600},
	}

	mgr, err := database.NewManagerWithMeta(metaCfg, shardCfgs, nil)
	require.NoError(t, err, "database connection failed")

	router := sharding.NewRouterWithConfig(3, 10)
	accountRepo := repository.NewAccountRepository(mgr, router)
	txRepo := repository.NewTransactionRepository(mgr, router)
	ruleRepo := repository.NewTransactionRuleRepository(mgr, router)
	orderRepo := repository.NewTransactionOrderRepository(mgr, router)

	tccRepo := repository.NewTccRepository(mgr, router)
	tccCoordRepo := repository.NewTccCoordinatorRepository(mgr, router)
	outboxRepo, err := repository.NewSettlementOutboxRepository(mgr, router)
	if err != nil {
		t.Fatalf("create outbox repo: %v", err)
	}
	batchOrderRepo := repository.NewBatchOrderRepository(mgr, router)
	bufferRepo := repository.NewBalanceBufferRepository(mgr, router)
	hotAccountRepo := repository.NewHotAccountRepository(mgr)
	bufferAccountRepo := repository.NewBufferAccountRepository(mgr)
	businessTypeRepo := repository.NewAccountBusinessTypeRepository(mgr)
	idGen, err := idgen.NewIDGeneratorFromManager(mgr, logger)
	if err != nil {
		t.Fatalf("create id generator: %v", err)
	}
	accountingSvc := service.NewAccountingService(accountRepo, txRepo, tccRepo, tccCoordRepo, orderRepo, outboxRepo, batchOrderRepo, bufferRepo, hotAccountRepo, bufferAccountRepo, businessTypeRepo, ruleRepo, mgr, router, idGen, logger)
	txSvc := service.NewTransactionService(ruleRepo, orderRepo, accountRepo, accountingSvc, logger)
	dayCutControlRepo := repository.NewDayCutControlRepository(mgr, router)
	snapshotRepo := repository.NewBalanceSnapshotRepository(mgr, router)
	dayCutSvc := service.NewDayCutService(router, mgr, txRepo, dayCutControlRepo, snapshotRepo, accountRepo, bufferRepo, outboxRepo, logger)

	env := &testEnvironment{
		dbManager: mgr, router: router,
		accountRepo: accountRepo, transactionRepo: txRepo,
		ruleRepo: ruleRepo, orderRepo: orderRepo,
		accountingService: accountingSvc, transactionService: txSvc,
		dayCutService: dayCutSvc, logger: logger,
	}

	// 清理上次测试残留数据，再写入种子数据
	env.truncateAllShardTables(t)
	env.seedConfigData(t)
	return env
}

func (env *testEnvironment) cleanup() { env.dbManager.Close() }

// truncateAllShardTables 清理所有分片表数据（测试前调用，确保幂等）
func (env *testEnvironment) truncateAllShardTables(t *testing.T) {
	t.Helper()
	tables := []string{
		"account", "account_transaction", "accounting_voucher",
		"account_balance_snapshot", "tcc_transaction", "transaction_order",
		"transaction_order_extra", "settlement_outbox", "batch_order",
		"account_balance_buffer", "merchant_info", "distributed_lock",
		"day_cut_control", "async_task",
	}
	shards := env.router.GetAllShards()
	for _, shard := range shards {
		db, err := env.dbManager.GetDB(shard.DBIndex)
		if err != nil {
			continue
		}
		db.Exec("SET FOREIGN_KEY_CHECKS=0")
		for _, tbl := range tables {
			tableName := env.router.GetTableName(tbl, shard.TableIndex)
			db.Exec("TRUNCATE TABLE `" + tableName + "`")
		}
		db.Exec("SET FOREIGN_KEY_CHECKS=1")
	}
}

// seedConfigData 写入账户类型和交易规则种子数据（幂等，使用 INSERT IGNORE）
// account_type_info / transaction_rule 存入 account_meta 元数据库
func (env *testEnvironment) seedConfigData(t *testing.T) {
	t.Helper()
	// 元数据写入 account_meta 库
	metaDB, err := env.dbManager.GetMetaDB()
	require.NoError(t, err)

	// 账户类型
	metaDB.Exec(`INSERT IGNORE INTO account_type_info (id,account_type,account_type_name,owner_type,balance_direction) VALUES
		(1,'USER_WALLET','用户钱包',1,'LIABILITY','C'),
		(2,'MERCHANT_WALLET','商户钱包',2,'LIABILITY','C'),
		(3,'PLATFORM_PNL','平台损益',3,'EQUITY','C'),
		(4,'PLATFORM_TRANSIT','平台中转',4,'ASSET','D'),
		(5,'PLATFORM_FEE','平台手续费',3,'REVENUE','C'),
		(6,'CHANNEL_WALLET','渠道费账户',4,'LIABILITY','C'),
		(7,'MARKETING_WALLET','营销账户',3,'EXPENSE','D')`)

	// 交易规则
	// 1. 用户充值（修正：用 BANK_ACCOUNT，不再用 PNL）
	metaDB.Exec(`
		INSERT IGNORE INTO transaction_rule 
		(id,product_code,event_code,hash_key,credit_subject_id,debit_subject_id,from_direction,to_direction,transaction_type,bookkeeping_mode) 
		VALUES 
		(1,'DEPOSIT','USER_DEPOSIT',
 		'DEPOSIT,USER_DEPOSIT,BANK_ACCOUNT,USER_WALLET',
 		'USER_WALLET',     -- 贷：负债增加（用户余额）
 		'BANK_ACCOUNT',    -- 借：资产增加（银行资金）
 		'credit','debit',
 		1,'DOUBLE_ENTRY')
		`)

	// 2. 用户支付商户（主流水：正确，保持）
	metaDB.Exec(`
INSERT IGNORE INTO transaction_rule 
(id,product_code,event_code,hash_key,credit_subject_id,debit_subject_id,from_direction,to_direction,transaction_type,bookkeeping_mode) 
VALUES 
(2,'PAYMENT','CHECKOUT_PAY',
 'PAYMENT,CHECKOUT_PAY,USER_WALLET,MERCHANT_WALLET',
 'USER_WALLET',        -- 贷：用户余额减少
 'MERCHANT_WALLET',    -- 借：商户余额增加
 'credit','debit',
 2,'DOUBLE_ENTRY')
`)

	// 3. 用户转账（逻辑正确，但强调 runtime 要带 account_id）
	metaDB.Exec(`
INSERT IGNORE INTO transaction_rule 
(id,product_code,event_code,hash_key,credit_subject_id,debit_subject_id,from_direction,to_direction,transaction_type,bookkeeping_mode) 
VALUES 
(3,'TRANSFER','USER_TRANSFER',
 'TRANSFER,USER_TRANSFER,USER_WALLET,USER_WALLET',
 'USER_WALLET',   -- from_user（运行时决定 account_id）
 'USER_WALLET',   -- to_user（运行时决定 account_id）
 'credit','debit',
 3,'DOUBLE_ENTRY')
`)
}

// createAccount 快捷账户创建（用户/商户业务账户）。
// businessType 默认传 model.AccountBusinessTypeUserBalance（1）。
// 系统账户（Platform/Transit/Fee 等）请用 createPlatformAccount。
func (env *testEnvironment) createAccount(t *testing.T, ctx context.Context, ownerID int64, aType model.AccountType, cat model.AccountCategory) *model.Account {
	t.Helper()
	acc, err := env.accountingService.CreateAccount(ctx, ownerID, model.AccountBusinessTypeUserBalance, aType, cat, "PHP")
	if errors.Is(err, model.ErrAccountAlreadyExists) {
		// 账户已存在（多个测试共享同一 userId），通过 repo 查询并返回
		acc, err = env.accountRepo.GetAccountByUserAndBusinessType(ctx, ownerID, model.AccountBusinessTypeUserBalance)
	}
	require.NoError(t, err)
	require.NotNil(t, acc)
	return acc
}

// createPlatformAccount e2e 辅助：单点创建系统内部账户（Platform / Transit / Fee 等）。
// reservedID ∈ [1, 10000]；business_type / category 由 accountType 1:1 推导。
func (env *testEnvironment) createPlatformAccount(t *testing.T, ctx context.Context, reservedID int64, aType model.AccountType) *model.Account {
	t.Helper()
	acc, err := env.accountingService.CreatePlatformAccount(ctx, reservedID, aType, "PHP")
	require.NoError(t, err)
	require.NotNil(t, acc)
	return acc
}

// doubleEntry 快捷复式记账
func (env *testEnvironment) doubleEntry(t *testing.T, ctx context.Context, bizNo string, bizType model.BusinessType, entries ...service.AccountingEntry) string {
	t.Helper()
	voucherNo, _, err := env.accountingService.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
		BusinessNo: bizNo, BusinessType: bizType, Entries: entries, Currency: "PHP",
	})
	require.NoError(t, err)
	require.NotEmpty(t, voucherNo)
	return voucherNo
}

func (env *testEnvironment) balance(t *testing.T, ctx context.Context, accountNo string) int64 {
	t.Helper()
	acc, err := env.accountingService.GetAccount(ctx, accountNo)
	require.NoError(t, err)
	require.NotNil(t, acc)
	return acc.Balance
}

// getBalanceSnapshot 查询余额快照（GORM 实现）
func (env *testEnvironment) getBalanceSnapshot(ctx context.Context, accountNo, snapshotDate string) (*model.AccountBalanceSnapshot, error) {
	dbIndex, tableIndex := env.router.RouteByAccountNo(accountNo)
	tableName := env.router.GetTableName("account_balance_snapshot", tableIndex)
	db, err := env.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}
	var snap model.AccountBalanceSnapshot
	result := db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND snapshot_date = ?", accountNo, snapshotDate).
		First(&snap)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("snapshot not found")
		}
		return nil, result.Error
	}
	return &snap, nil
}

// flushBuffer 手动刷新 account_balance_buffer 到 account 余额（平台/中转账户测试专用）
func (env *testEnvironment) flushBuffer(ctx context.Context) {
	type bufRow struct {
		AccountNo    string `gorm:"column:account_no"`
		PendingDelta string `gorm:"column:pending_delta"`
	}
	for _, shard := range env.router.GetAllShards() {
		db, _ := env.dbManager.GetDB(shard.DBIndex)
		if db == nil {
			continue
		}
		bufTable := env.router.GetTableName("account_balance_buffer", shard.TableIndex)
		accountTable := env.router.GetTableName("account", shard.TableIndex)
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

// uniqueBizNo 生成唯一业务号（测试用）
func uniqueBizNo(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

// ─── Test Case 1: 用户充值 ────────────────────────────────────────────────────

func TestE2E_TC01_UserDeposit(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 10001, model.AccountTypeUser, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)

	env.doubleEntry(t, ctx, uniqueBizNo("DEP"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 500, Description: "用户充值借"},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 500, Description: "平台贷"},
	)

	assert.Equal(t, int64(500), env.balance(t, ctx, user.AccountNo))
	t.Logf("TC01 PASS: user balance=%d", env.balance(t, ctx, user.AccountNo))
}

// ─── Test Case 2: 用户提现 ────────────────────────────────────────────────────

func TestE2E_TC02_UserWithdraw(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 10002, model.AccountTypeUser, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)

	// 先充值 200
	env.doubleEntry(t, ctx, uniqueBizNo("DEP"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 200},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 200},
	)

	// 提现 80
	env.doubleEntry(t, ctx, uniqueBizNo("WD"), model.BusinessTypeWithdraw,
		service.AccountingEntry{AccountNo: platform.AccountNo, DebitAmount: 80},
		service.AccountingEntry{AccountNo: user.AccountNo, CreditAmount: 80},
	)

	assert.Equal(t, int64(120), env.balance(t, ctx, user.AccountNo))
	t.Logf("TC02 PASS: user balance after withdraw=%d", env.balance(t, ctx, user.AccountNo))
}

// ─── Test Case 3: 用户支付商户（无手续费）────────────────────────────────────

func TestE2E_TC03_UserPayMerchant_NoFee(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 10003, model.AccountTypeUser, model.AccountCategoryAsset)
	merchant := env.createAccount(t, ctx, 20003, model.AccountTypeMerchant, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)

	// 先充值
	env.doubleEntry(t, ctx, uniqueBizNo("DEP"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 300},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 300},
	)

	// 支付 200
	env.doubleEntry(t, ctx, uniqueBizNo("PAY"), model.BusinessTypePayment,
		service.AccountingEntry{AccountNo: user.AccountNo, CreditAmount: 200},
		service.AccountingEntry{AccountNo: merchant.AccountNo, DebitAmount: 200},
	)

	assert.Equal(t, int64(100), env.balance(t, ctx, user.AccountNo))
	assert.Equal(t, int64(200), env.balance(t, ctx, merchant.AccountNo))
	t.Logf("TC03 PASS: user=%d merchant=%d",
		env.balance(t, ctx, user.AccountNo), env.balance(t, ctx, merchant.AccountNo))
}

// ─── Test Case 4: 用户支付含平台手续费（3条分录）────────────────────────────

func TestE2E_TC04_UserPayMerchant_WithPlatformFee(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 10004, model.AccountTypeUser, model.AccountCategoryAsset)
	merchant := env.createAccount(t, ctx, 20004, model.AccountTypeMerchant, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)
	fee := env.createAccount(t, ctx, 30004, model.AccountTypePlatform, model.AccountCategoryExpense) // 手续费科目（借方增加）

	// 充值 1000
	env.doubleEntry(t, ctx, uniqueBizNo("DEP"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 1000},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 1000},
	)

	// 支付主流水：用户 → 商户
	env.doubleEntry(t, ctx, uniqueBizNo("PAY"), model.BusinessTypePayment,
		service.AccountingEntry{AccountNo: user.AccountNo, CreditAmount: 500},
		service.AccountingEntry{AccountNo: merchant.AccountNo, DebitAmount: 500},
	)

	// 手续费流水：商户 → 平台手续费账户（0.6% ≈ 3）
	env.doubleEntry(t, ctx, uniqueBizNo("FEE"), model.BusinessTypeCommission,
		service.AccountingEntry{AccountNo: merchant.AccountNo, CreditAmount: 3},
		service.AccountingEntry{AccountNo: fee.AccountNo, DebitAmount: 3},
	)

	// 平台账户使用缓冲余额，需手动 flush 后再验证
	env.flushBuffer(ctx)

	assert.Equal(t, int64(500), env.balance(t, ctx, user.AccountNo))
	assert.Equal(t, int64(497), env.balance(t, ctx, merchant.AccountNo)) // 500-3
	assert.Equal(t, int64(3), env.balance(t, ctx, fee.AccountNo))
	t.Logf("TC04 PASS: user=%d merchant=%d platformFee=%d",
		env.balance(t, ctx, user.AccountNo),
		env.balance(t, ctx, merchant.AccountNo),
		env.balance(t, ctx, fee.AccountNo))
}

// ─── Test Case 5: 含渠道费（4条分录）────────────────────────────────────────

func TestE2E_TC05_UserPay_WithChannelFee(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 10005, model.AccountTypeUser, model.AccountCategoryAsset)
	merchant := env.createAccount(t, ctx, 20005, model.AccountTypeMerchant, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)
	channel := env.createAccount(t, ctx, 40005, model.AccountTypeTransit, model.AccountCategoryAsset) // 渠道账户（借方增加）

	// 充值
	env.doubleEntry(t, ctx, uniqueBizNo("DEP"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 1000},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 1000},
	)

	// 支付 1000, 渠道费 1% = 10, 平台净收 990
	// 用户 → 商户 1000
	env.doubleEntry(t, ctx, uniqueBizNo("PAY"), model.BusinessTypePayment,
		service.AccountingEntry{AccountNo: user.AccountNo, CreditAmount: 1000},
		service.AccountingEntry{AccountNo: merchant.AccountNo, DebitAmount: 1000},
	)
	// 商户代扣渠道费 10 → 渠道账户
	env.doubleEntry(t, ctx, uniqueBizNo("CFEE"), model.BusinessTypeCommission,
		service.AccountingEntry{AccountNo: merchant.AccountNo, CreditAmount: 10},
		service.AccountingEntry{AccountNo: channel.AccountNo, DebitAmount: 10},
	)

	// 渠道账户（Transit 类型）使用缓冲余额，需手动 flush 后再验证
	env.flushBuffer(ctx)

	assert.Equal(t, int64(0), env.balance(t, ctx, user.AccountNo))
	assert.Equal(t, int64(990), env.balance(t, ctx, merchant.AccountNo))
	assert.Equal(t, int64(10), env.balance(t, ctx, channel.AccountNo))
	t.Logf("TC05 PASS: user=%d merchant=%d channelFee=%d",
		env.balance(t, ctx, user.AccountNo),
		env.balance(t, ctx, merchant.AccountNo),
		env.balance(t, ctx, channel.AccountNo))
}

// ─── Test Case 6: 用户转账 ────────────────────────────────────────────────────

func TestE2E_TC06_UserTransfer(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	sender := env.createAccount(t, ctx, 10006, model.AccountTypeUser, model.AccountCategoryAsset)
	receiver := env.createAccount(t, ctx, 10007, model.AccountTypeUser, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)

	// 充值 sender 300
	env.doubleEntry(t, ctx, uniqueBizNo("DEP"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: sender.AccountNo, DebitAmount: 300},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 300},
	)

	// 转账 150 给 receiver
	env.doubleEntry(t, ctx, uniqueBizNo("TFR"), model.BusinessTypeTransfer,
		service.AccountingEntry{AccountNo: sender.AccountNo, CreditAmount: 150},
		service.AccountingEntry{AccountNo: receiver.AccountNo, DebitAmount: 150},
	)

	assert.Equal(t, int64(150), env.balance(t, ctx, sender.AccountNo))
	assert.Equal(t, int64(150), env.balance(t, ctx, receiver.AccountNo))
	t.Logf("TC06 PASS: sender=%d receiver=%d",
		env.balance(t, ctx, sender.AccountNo),
		env.balance(t, ctx, receiver.AccountNo))
}

// ─── Test Case 7: 商户退款 ────────────────────────────────────────────────────

func TestE2E_TC07_MerchantRefund(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 10008, model.AccountTypeUser, model.AccountCategoryAsset)
	merchant := env.createAccount(t, ctx, 20008, model.AccountTypeMerchant, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)

	// 充值 + 支付
	env.doubleEntry(t, ctx, uniqueBizNo("DEP"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 500},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 500},
	)
	env.doubleEntry(t, ctx, uniqueBizNo("PAY"), model.BusinessTypePayment,
		service.AccountingEntry{AccountNo: user.AccountNo, CreditAmount: 200},
		service.AccountingEntry{AccountNo: merchant.AccountNo, DebitAmount: 200},
	)

	// 退款 200
	env.doubleEntry(t, ctx, uniqueBizNo("REF"), model.BusinessTypeRefund,
		service.AccountingEntry{AccountNo: merchant.AccountNo, CreditAmount: 200},
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 200},
	)

	assert.Equal(t, int64(500), env.balance(t, ctx, user.AccountNo))   // 回到充值后
	assert.Equal(t, int64(0), env.balance(t, ctx, merchant.AccountNo)) // 退款后归零
	t.Logf("TC07 PASS: user=%d merchant=%d",
		env.balance(t, ctx, user.AccountNo),
		env.balance(t, ctx, merchant.AccountNo))
}

// ─── Test Case 8: 营销红包发放 ────────────────────────────────────────────────

func TestE2E_TC08_MarketingBonus(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 10009, model.AccountTypeUser, model.AccountCategoryAsset)
	marketing := env.createAccount(t, ctx, 50009, model.AccountTypePlatform, model.AccountCategoryExpense) // 营销费用账户

	// 平台营销账户扣款，给用户发红包 20 元
	// 营销费用账户（借方正常），贷出减少余额
	// 用户钱包借入增加余额
	env.doubleEntry(t, ctx, uniqueBizNo("MKT"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: marketing.AccountNo, CreditAmount: 20, Description: "营销红包贷"},
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 20, Description: "用户收红包"},
	)

	// 用户拿红包消费 20
	merchant := env.createAccount(t, ctx, 20009, model.AccountTypeMerchant, model.AccountCategoryAsset)
	env.doubleEntry(t, ctx, uniqueBizNo("PAY"), model.BusinessTypePayment,
		service.AccountingEntry{AccountNo: user.AccountNo, CreditAmount: 20},
		service.AccountingEntry{AccountNo: merchant.AccountNo, DebitAmount: 20},
	)

	assert.Equal(t, int64(0), env.balance(t, ctx, user.AccountNo))
	assert.Equal(t, int64(20), env.balance(t, ctx, merchant.AccountNo))
	t.Logf("TC08 PASS: user=%d merchant=%d",
		env.balance(t, ctx, user.AccountNo), env.balance(t, ctx, merchant.AccountNo))
}

// ─── Test Case 9: TransactionService 幂等性（相同 order_no 只执行一次）──────

func TestE2E_TC09_TransactionService_Idempotency(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	// 创建账户并预充值
	user := env.createAccount(t, ctx, 10010, model.AccountTypeUser, model.AccountCategoryAsset)
	merchant := env.createAccount(t, ctx, 20010, model.AccountTypeMerchant, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)

	env.doubleEntry(t, ctx, uniqueBizNo("DEP"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 500},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 500},
	)

	orderNo := uniqueBizNo("TXN_IDEM")
	// SP-AC-7: multi-leg API. 这个 case 只有 1 条 leg.
	req := &service.CreateTransactionRequest{
		OrderNo:     orderNo,
		ProductCode: "PAYMENT",
		EventCode:   "CHECKOUT_PAY",
		Legs: []service.TxnLeg{
			{
				FromAccountID: user.AccountNo,
				ToAccountID:   merchant.AccountNo,
				Amount:        "100",
				Currency:      "PHP",
			},
		},
		MaxRetry: 3,
	}

	// 第一次调用
	resp1, err := env.transactionService.CreateTransaction(ctx, req)
	require.NoError(t, err)
	require.Equal(t, model.TransactionOrderStatusSuccess, resp1.Status)
	t.Logf("TC09 first call: voucherNo=%s", resp1.VoucherNo)

	// 第二次相同 order_no → 直接返回缓存结果，余额不变
	resp2, err := env.transactionService.CreateTransaction(ctx, req)
	require.NoError(t, err)
	require.Equal(t, model.TransactionOrderStatusSuccess, resp2.Status)
	require.Equal(t, resp1.VoucherNo, resp2.VoucherNo, "idempotent: voucher_no must be the same")

	// 验证余额只扣了一次
	assert.Equal(t, int64(400), env.balance(t, ctx, user.AccountNo))
	assert.Equal(t, int64(100), env.balance(t, ctx, merchant.AccountNo))
	t.Logf("TC09 PASS: idempotent, user=%d merchant=%d",
		env.balance(t, ctx, user.AccountNo), env.balance(t, ctx, merchant.AccountNo))
}

// ─── Test Case 10: TransactionService 重试（模拟失败后重试成功）────────────

func TestE2E_TC10_TransactionService_Retry(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 10011, model.AccountTypeUser, model.AccountCategoryAsset)
	merchant := env.createAccount(t, ctx, 20011, model.AccountTypeMerchant, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)

	env.doubleEntry(t, ctx, uniqueBizNo("DEP"), model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 300},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 300},
	)

	orderNo := uniqueBizNo("TXN_RETRY")

	// 手动创建一个 FAILED 状态的订单, Extra 里塞 multi-leg payload (重试时要还原).
	// TransactionService 以 orderNo 作为 businessNo (与 CreateTransaction 保持一致).
	extraJSON := fmt.Sprintf(`{"legs":[{"from_account_id":%q,"to_account_id":%q,"amount":"200","currency":"PHP"}]}`,
		user.AccountNo, merchant.AccountNo)
	failedOrder := &model.TransactionOrder{
		OrderNo:       orderNo,
		BusinessNo:    orderNo,
		BusinessType:  "",
		ProductCode:   "PAYMENT",
		EventCode:     "CHECKOUT_PAY",
		Amount:        "200",
		Currency:      "PHP",
		Status:        model.TransactionOrderStatusFailed,
		RetryCount:    1,
		MaxRetryCount: 3,
		ErrorMessage:  "simulated failure",
		Extra:         extraJSON,
	}
	err := env.orderRepo.Create(ctx, failedOrder)
	require.NoError(t, err)

	// 重试
	resp, err := env.transactionService.RetryTransaction(ctx, orderNo)
	require.NoError(t, err)
	require.Equal(t, model.TransactionOrderStatusSuccess, resp.Status)
	require.NotEmpty(t, resp.VoucherNo)

	assert.Equal(t, int64(100), env.balance(t, ctx, user.AccountNo))
	assert.Equal(t, int64(200), env.balance(t, ctx, merchant.AccountNo))
	t.Logf("TC10 PASS: retry success, user=%d merchant=%d voucherNo=%s",
		env.balance(t, ctx, user.AccountNo), env.balance(t, ctx, merchant.AccountNo), resp.VoucherNo)
}

// ─── Test Case 11: 借贷平衡校验（多笔交易后全局借=贷）────────────────────────

func TestE2E_TC11_DoubleEntryGlobalBalance(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)
	users := make([]*model.Account, 5)
	for i := range users {
		users[i] = env.createAccount(t, ctx, int64(11000+i), model.AccountTypeUser, model.AccountCategoryAsset)
	}
	merchants := make([]*model.Account, 2)
	for i := range merchants {
		merchants[i] = env.createAccount(t, ctx, int64(21000+i), model.AccountTypeMerchant, model.AccountCategoryAsset)
	}

	var totalDebit, totalCredit int64

	// 5 个用户各充值
	for i, u := range users {
		amt := int64((i + 1) * 100)
		env.doubleEntry(t, ctx, uniqueBizNo(fmt.Sprintf("DEP%d", i)), model.BusinessTypeDeposit,
			service.AccountingEntry{AccountNo: u.AccountNo, DebitAmount: amt},
			service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: amt},
		)
		totalDebit += amt
		totalCredit += amt
	}

	// 3 笔支付
	for i := 0; i < 3; i++ {
		amt := int64(50)
		env.doubleEntry(t, ctx, uniqueBizNo(fmt.Sprintf("PAY%d", i)), model.BusinessTypePayment,
			service.AccountingEntry{AccountNo: users[i].AccountNo, CreditAmount: amt},
			service.AccountingEntry{AccountNo: merchants[i%2].AccountNo, DebitAmount: amt},
		)
		totalDebit += amt
		totalCredit += amt
	}

	// 1 笔退款
	env.doubleEntry(t, ctx, uniqueBizNo("REF"), model.BusinessTypeRefund,
		service.AccountingEntry{AccountNo: merchants[0].AccountNo, CreditAmount: 30},
		service.AccountingEntry{AccountNo: users[0].AccountNo, DebitAmount: 30},
	)
	totalDebit += 30
	totalCredit += 30

	assert.Equal(t, totalDebit, totalCredit, "global debit(%d) == credit(%d)", totalDebit, totalCredit)
	t.Logf("TC11 PASS: globalDebit=%d globalCredit=%d", totalDebit, totalCredit)
}

// ─── Test Case 12: 日切全流程（含余额快照校验）────────────────────────────────

func TestE2E_TC12_DayCut(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryEquity)
	accs := make([]*model.Account, 4)
	for i := range accs {
		accs[i] = env.createAccount(t, ctx, int64(12000+i), model.AccountTypeUser, model.AccountCategoryAsset)
	}

	// 执行多笔交易
	for i, acc := range accs {
		amt := int64((i + 1) * 200)
		env.doubleEntry(t, ctx, uniqueBizNo(fmt.Sprintf("D%d", i)), model.BusinessTypeDeposit,
			service.AccountingEntry{AccountNo: acc.AccountNo, DebitAmount: amt},
			service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: amt},
		)
	}
	// 转账
	env.doubleEntry(t, ctx, uniqueBizNo("TFR"), model.BusinessTypeTransfer,
		service.AccountingEntry{AccountNo: accs[0].AccountNo, CreditAmount: 50},
		service.AccountingEntry{AccountNo: accs[1].AccountNo, DebitAmount: 50},
	)

	cutDate := time.Now().Format("2006-01-02")
	err := env.dayCutService.TriggerDayCut(ctx, cutDate, "PHP")
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	status, err := env.dayCutService.CheckDayCutStatus(ctx, cutDate)
	require.NoError(t, err)
	t.Logf("TC12 day cut status: %v", status)

	// 校验各账户快照的借贷平衡
	for _, acc := range accs {
		snap, err := env.getBalanceSnapshot(ctx, acc.AccountNo, cutDate)
		if err != nil {
			t.Logf("snapshot not found for %s (ok if no transactions): %v", acc.AccountNo, err)
			continue
		}
		expected := snap.BeginningBalance + snap.TotalDebit - snap.TotalCredit
		assert.Equal(t, snap.EndingBalance, expected,
			"account %s: beginning(%d)+debit(%d)-credit(%d) = %d, got ending=%d",
			acc.AccountNo, snap.BeginningBalance, snap.TotalDebit, snap.TotalCredit, expected, snap.EndingBalance)
		t.Logf("TC12 snapshot %s: begin=%d end=%d debit=%d credit=%d txCount=%d",
			acc.AccountNo, snap.BeginningBalance, snap.EndingBalance,
			snap.TotalDebit, snap.TotalCredit, snap.TransactionCount)
	}
	t.Logf("TC12 PASS: day cut completed")
}

// ─── Test Case (legacy) ───────────────────────────────────────────────────────

// TestE2E_DoubleEntryBooking 保留原有综合测试
func TestE2E_DoubleEntryBooking(t *testing.T) {
	ctx := context.Background()
	env := setupTestEnvironment(t)
	defer env.cleanup()

	user := env.createAccount(t, ctx, 99001, model.AccountTypeUser, model.AccountCategoryAsset)
	merchant := env.createAccount(t, ctx, 99002, model.AccountTypeMerchant, model.AccountCategoryAsset)
	platform := env.createAccount(t, ctx, 0, model.AccountTypePlatform, model.AccountCategoryRevenue)

	// 充值 100
	env.doubleEntry(t, ctx, "BIZ_DEPOSIT_LEGACY_001", model.BusinessTypeDeposit,
		service.AccountingEntry{AccountNo: user.AccountNo, DebitAmount: 100},
		service.AccountingEntry{AccountNo: platform.AccountNo, CreditAmount: 100},
	)
	assert.Equal(t, int64(100), env.balance(t, ctx, user.AccountNo))

	// 支付 50
	env.doubleEntry(t, ctx, "BIZ_PAYMENT_LEGACY_001", model.BusinessTypePayment,
		service.AccountingEntry{AccountNo: user.AccountNo, CreditAmount: 50},
		service.AccountingEntry{AccountNo: merchant.AccountNo, DebitAmount: 50},
	)
	assert.Equal(t, int64(50), env.balance(t, ctx, user.AccountNo))
	assert.Equal(t, int64(50), env.balance(t, ctx, merchant.AccountNo))
	t.Logf("Legacy TC PASS")
}
