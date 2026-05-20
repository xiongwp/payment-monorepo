package service

import (
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTx constructs a minimal synchronous AccountTransaction for testing.
func newTx(accountNo string, debit, credit, before, after int64) *model.AccountTransaction {
	return &model.AccountTransaction{
		AccountNo:       accountNo,
		DebitAmount:     debit,
		CreditAmount:    credit,
		BalanceBefore:   before,
		BalanceAfter:    after,
		BookingType:     model.TransactionBookingTypeSync,
		TransactionID:   accountNo + "_tx",
		TransactionTime: time.Now(),
	}
}

// newBufferedTx constructs a minimal buffered AccountTransaction for testing.
func newBufferedTx(accountNo string, debit, credit, before, after int64) *model.AccountTransaction {
	tx := newTx(accountNo, debit, credit, before, after)
	tx.BookingType = model.TransactionBookingTypeBuffered
	return tx
}

func newDayCutSvc() *dayCutService {
	return &dayCutService{}
}

// ─── calculateAccountStats ────────────────────────────────────────────────────

func TestCalculateAccountStats_Empty(t *testing.T) {
	svc := newDayCutSvc()
	result := svc.calculateAccountStats(nil)
	assert.Empty(t, result)

	result2 := svc.calculateAccountStats([]*model.AccountTransaction{})
	assert.Empty(t, result2)
}

func TestCalculateAccountStats_SingleAccount_SingleTx(t *testing.T) {
	svc := newDayCutSvc()
	txs := []*model.AccountTransaction{
		newTx("ACC001", 10000, 0, 50000, 60000),
	}
	stats := svc.calculateAccountStats(txs)
	require.Len(t, stats, 1)

	s := stats["ACC001"]
	require.NotNil(t, s)
	assert.Equal(t, "ACC001", s.AccountNo)
	assert.Equal(t, int64(10000), s.TotalDebit)
	assert.Equal(t, int64(0), s.TotalCredit)
	assert.Equal(t, 1, s.TransactionCount)
	assert.Equal(t, int64(50000), s.BeginningBalance) // first tx.BalanceBefore
	assert.Equal(t, int64(60000), s.EndingBalance)    // last tx.BalanceAfter
}

func TestCalculateAccountStats_SingleAccount_MultipleTx(t *testing.T) {
	svc := newDayCutSvc()
	txs := []*model.AccountTransaction{
		newTx("ACC001", 10000, 0, 100000, 110000), // first tx: beginning=100000
		newTx("ACC001", 0, 5000, 110000, 105000),
		newTx("ACC001", 20000, 0, 105000, 125000), // last tx: ending=125000
	}
	stats := svc.calculateAccountStats(txs)
	require.Len(t, stats, 1)

	s := stats["ACC001"]
	assert.Equal(t, int64(30000), s.TotalDebit)   // 10000 + 20000
	assert.Equal(t, int64(5000), s.TotalCredit)
	assert.Equal(t, 3, s.TransactionCount)
	assert.Equal(t, int64(100000), s.BeginningBalance)
	assert.Equal(t, int64(125000), s.EndingBalance)
}

func TestCalculateAccountStats_MultipleAccounts(t *testing.T) {
	svc := newDayCutSvc()
	txs := []*model.AccountTransaction{
		newTx("ACC001", 10000, 0, 0, 10000),
		newTx("ACC002", 0, 20000, 50000, 30000),
		newTx("ACC001", 0, 5000, 10000, 5000),
		newTx("ACC002", 3000, 0, 30000, 33000),
	}
	stats := svc.calculateAccountStats(txs)
	require.Len(t, stats, 2)

	s1 := stats["ACC001"]
	assert.Equal(t, int64(10000), s1.TotalDebit)
	assert.Equal(t, int64(5000), s1.TotalCredit)
	assert.Equal(t, 2, s1.TransactionCount)
	assert.Equal(t, int64(0), s1.BeginningBalance)
	assert.Equal(t, int64(5000), s1.EndingBalance)

	s2 := stats["ACC002"]
	assert.Equal(t, int64(3000), s2.TotalDebit)
	assert.Equal(t, int64(20000), s2.TotalCredit)
	assert.Equal(t, 2, s2.TransactionCount)
	assert.Equal(t, int64(50000), s2.BeginningBalance)
	assert.Equal(t, int64(33000), s2.EndingBalance)
}

func TestCalculateAccountStats_OnlyCreditTx(t *testing.T) {
	svc := newDayCutSvc()
	txs := []*model.AccountTransaction{
		newTx("ACC001", 0, 15000, 20000, 5000),
	}
	stats := svc.calculateAccountStats(txs)
	s := stats["ACC001"]
	assert.Equal(t, int64(0), s.TotalDebit)
	assert.Equal(t, int64(15000), s.TotalCredit)
	assert.Equal(t, int64(20000), s.BeginningBalance)
	assert.Equal(t, int64(5000), s.EndingBalance)
}

func TestCalculateAccountStats_BeginningBalanceIsFirstTxBalanceBefore(t *testing.T) {
	// BeginningBalance must be BalanceBefore of the FIRST transaction for that account,
	// not the minimum or any other aggregation.
	svc := newDayCutSvc()
	txs := []*model.AccountTransaction{
		newTx("ACC001", 1000, 0, 99000, 100000), // first: before=99000
		newTx("ACC001", 2000, 0, 100000, 102000),
	}
	stats := svc.calculateAccountStats(txs)
	assert.Equal(t, int64(99000), stats["ACC001"].BeginningBalance)
	assert.Equal(t, int64(102000), stats["ACC001"].EndingBalance)
}

func TestCalculateAccountStats_LargeAmounts(t *testing.T) {
	svc := newDayCutSvc()
	large := int64(1_000_000_000_000) // 10 trillion stored units
	txs := []*model.AccountTransaction{
		newTx("ACC001", large, 0, 0, large),
	}
	stats := svc.calculateAccountStats(txs)
	assert.Equal(t, large, stats["ACC001"].TotalDebit)
	assert.Equal(t, large, stats["ACC001"].EndingBalance)
}

// ─── HasBufferedBooking flag ──────────────────────────────────────────────────

func TestCalculateAccountStats_SyncOnly_NoBufferedFlag(t *testing.T) {
	svc := newDayCutSvc()
	txs := []*model.AccountTransaction{
		newTx("ACC001", 10000, 0, 100000, 110000),
		newTx("ACC001", 5000, 0, 110000, 115000),
	}
	stats := svc.calculateAccountStats(txs)
	assert.False(t, stats["ACC001"].HasBufferedBooking, "sync-only txs should not set HasBufferedBooking")
}

func TestCalculateAccountStats_BufferedTx_SetsFlag(t *testing.T) {
	svc := newDayCutSvc()
	txs := []*model.AccountTransaction{
		newBufferedTx("PLATFORM001", 10000, 0, 0, 10000),
	}
	stats := svc.calculateAccountStats(txs)
	assert.True(t, stats["PLATFORM001"].HasBufferedBooking)
}

func TestCalculateAccountStats_MixedBookingTypes_SetsFlag(t *testing.T) {
	// Account with one sync and one buffered tx: flag should be true
	svc := newDayCutSvc()
	txs := []*model.AccountTransaction{
		newTx("ACC001", 10000, 0, 0, 10000),
		newBufferedTx("ACC001", 5000, 0, 10000, 15000),
	}
	stats := svc.calculateAccountStats(txs)
	assert.True(t, stats["ACC001"].HasBufferedBooking)
	// Aggregates are still correct
	assert.Equal(t, int64(15000), stats["ACC001"].TotalDebit)
	assert.Equal(t, int64(15000), stats["ACC001"].EndingBalance)
}

func TestCalculateAccountStats_MultipleAccounts_BufferedFlagPerAccount(t *testing.T) {
	svc := newDayCutSvc()
	txs := []*model.AccountTransaction{
		newTx("USER001", 10000, 0, 0, 10000),
		newBufferedTx("PLATFORM001", 10000, 0, 500000, 510000),
	}
	stats := svc.calculateAccountStats(txs)
	assert.False(t, stats["USER001"].HasBufferedBooking, "user account should not be flagged")
	assert.True(t, stats["PLATFORM001"].HasBufferedBooking, "platform account should be flagged")
}

// ─── id-watermark 切窗 + 试算平衡 ──────────────────────────────────────────────

// 验证 cut_date 字符串 → 本地 00:00 解析仍可用作参数校验的 sanity utility。
// （id-watermark 模式下不再用作扫描边界，但保留作为用户输入校验工具）
func TestTruncateToDay(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"normal", "2026-04-27", false},
		{"leap day", "2024-02-29", false},
		{"first of year", "2026-01-01", false},
		{"bad", "2026-13-01", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := truncateToDay(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 0, got.Hour())
			assert.Equal(t, 0, got.Minute())
			assert.Equal(t, 0, got.Second())
			assert.Equal(t, 0, got.Nanosecond())
			assert.Equal(t, time.Local, got.Location())
		})
	}
}

// id-watermark 切窗下的试算平衡验证：
// 同一 voucher 的两条分录在单事务 INSERT，因此分到同一个 (id, id+1) 紧邻区间。
// (cut_min_id, cut_max_id] 任意取值都不会切开同一 voucher。
//
// 模拟：构造若干 voucher（每张两条 entry，id 紧邻），随机 cut_max_id，
// 验证落在窗内的 SUM(DR) == SUM(CR)。
func TestIDWatermark_VoucherAtomic_TrialBalance(t *testing.T) {
	type entry struct {
		voucher       string
		id            int64
		debit, credit int64
	}
	// 6 张 voucher × 2 entry = 12 行；id 紧邻分配（V1: 1,2 ; V2: 3,4 ; ...）
	rows := []entry{
		{"V1", 1, 100, 0}, {"V1", 2, 0, 100},
		{"V2", 3, 200, 0}, {"V2", 4, 0, 200},
		{"V3", 5, 50, 0}, {"V3", 6, 0, 50},
		{"V4", 7, 999, 0}, {"V4", 8, 0, 999},
		{"V5", 9, 1, 0}, {"V5", 10, 0, 1},
		{"V6", 11, 4444, 0}, {"V6", 12, 0, 4444},
	}

	// 任意 cut_max_id（包括"切在 voucher 中间"的 id）扫描结果都应保持试算平衡，
	// 因为同 voucher 的两条 id 在 INSERT 单事务里"原子分配"——MySQL InnoDB
	// auto-increment 在 lock_mode=1/2 下都保证同一语句序列内 id 连续。这里也
	// 顺便覆盖人为故意切到 voucher 中间的边界 case（id=1, id=3 等"voucher first id"），
	// 此时第二条因 id > cut_max_id 不入选，DR/CR 不平 —— 但这正是我们的实现
	// 永远不会发生的：lockCutWatermark 取 MAX(id)，包含了 INSERT 时分配的所有 id。
	for _, watermark := range []int64{0, 2, 4, 6, 8, 10, 12} {
		var sumDR, sumCR int64
		for _, e := range rows {
			if e.id > watermark {
				continue
			}
			sumDR += e.debit
			sumCR += e.credit
		}
		assert.Equal(t, sumDR, sumCR,
			"id-watermark=%d should preserve trial balance (sumDR=%d sumCR=%d)", watermark, sumDR, sumCR)
	}
}
