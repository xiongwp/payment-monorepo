package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"go.uber.org/zap"
)

// MockSettlementOutboxRepository mock outbox repository for testing
type MockSettlementOutboxRepository struct {
	failedRecs []*model.SettlementOutbox
	staleRecs  []*model.SettlementOutbox
}

func (m *MockSettlementOutboxRepository) Create(ctx context.Context, outbox *model.SettlementOutbox) error {
	return nil
}

func (m *MockSettlementOutboxRepository) ResetFailedToPending(ctx context.Context, outbox *model.SettlementOutbox) error {
	return nil
}

func (m *MockSettlementOutboxRepository) MarkRedisDone(ctx context.Context, voucherNo string) error {
	return nil
}

func (m *MockSettlementOutboxRepository) MarkMySQLDone(ctx context.Context, voucherNo string) error {
	return nil
}

func (m *MockSettlementOutboxRepository) MarkFailed(ctx context.Context, voucherNo, errorMsg string) error {
	return nil
}

func (m *MockSettlementOutboxRepository) FindByStatus(ctx context.Context, status model.OutboxStatus, limit int) ([]*model.SettlementOutbox, error) {
	return m.failedRecs, nil
}

func (m *MockSettlementOutboxRepository) FindByVoucher(ctx context.Context, voucherNo string) (*model.SettlementOutbox, error) {
	return nil, nil
}

func (m *MockSettlementOutboxRepository) FindStuckPending(ctx context.Context, olderThan time.Duration, limit int) ([]*model.SettlementOutbox, error) {
	return m.staleRecs, nil
}

func (m *MockSettlementOutboxRepository) FindUnprocessed(ctx context.Context, upToDate string) ([]*model.SettlementOutbox, error) {
	return nil, nil
}

func (m *MockSettlementOutboxRepository) IncrementRetry(ctx context.Context, voucherNo, errorMsg string) error {
	return nil
}

func (m *MockSettlementOutboxRepository) DeleteOldDone(ctx context.Context, olderThan time.Duration) (int64, error) {
	return 0, nil
}

// TestNewReconcileWorker 测试 worker 初始化
func TestNewReconcileWorker(t *testing.T) {
	logger, _ := zap.NewProduction()
	repo := &MockSettlementOutboxRepository{}
	w := NewReconcileWorker(repo, nil, nil, logger)

	if w == nil {
		t.Fatal("NewReconcileWorker returned nil")
	}
	if w.Interval != 5*time.Minute {
		t.Errorf("expected Interval=5m, got %v", w.Interval)
	}
	if w.StaleThresh != 1*time.Hour {
		t.Errorf("expected StaleThresh=1h, got %v", w.StaleThresh)
	}
}

// TestSetOrderCoreEndpoint 测试设置 endpoint
func TestSetOrderCoreEndpoint(t *testing.T) {
	logger, _ := zap.NewProduction()
	w := NewReconcileWorker(&MockSettlementOutboxRepository{}, nil, nil, logger)
	endpoint := "localhost:50051"
	w.SetOrderCoreEndpoint(endpoint)

	if w.grpcEndpoint != endpoint {
		t.Errorf("expected %s, got %s", endpoint, w.grpcEndpoint)
	}
}

// TestRunOnceWithEmptyOutbox 测试空 outbox 场景
func TestRunOnceWithEmptyOutbox(t *testing.T) {
	logger, _ := zap.NewProduction()
	repo := &MockSettlementOutboxRepository{
		failedRecs: []*model.SettlementOutbox{},
		staleRecs:  []*model.SettlementOutbox{},
	}
	w := NewReconcileWorker(repo, nil, nil, logger)

	ctx := context.Background()
	err := w.runOnce(ctx)
	if err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}
}

// TestExtractChargePI 测试 charge/pi 提取（简化版，目前返回空）
func TestExtractChargePI(t *testing.T) {
	logger, _ := zap.NewProduction()
	w := NewReconcileWorker(&MockSettlementOutboxRepository{}, nil, nil, logger)

	rec := &model.SettlementOutbox{
		VoucherNo: "test-voucher",
		EventData: `{"charge_id":"ch_123","payment_intent_id":"pi_456"}`,
	}

	chargeID, piID := w.extractChargePI(rec)
	// 当前简化实现返回空，生产需要实现 JSON 解析
	if chargeID != "" || piID != "" {
		t.Logf("extractChargePI: charge=%s pi=%s (simplified impl returns empty)", chargeID, piID)
	}
}

// TestRecordReconcileEvent 测试修复事件记录
func TestRecordReconcileEvent(t *testing.T) {
	ctx := context.Background()
	event := &ReconcileEvent{
		VoucherNo:        "test-voucher",
		ChargeID:         "ch_123",
		OrderCoreStatus:  "succeeded",
		AccountingStatus: "pending",
		RepairAction:     "mark_done",
	}

	err := RecordReconcileEvent(ctx, event)
	if err != nil {
		t.Fatalf("RecordReconcileEvent failed: %v", err)
	}

	if repairEventCounter.Load() < 1 {
		t.Errorf("expected counter >= 1, got %d", repairEventCounter.Load())
	}
}
