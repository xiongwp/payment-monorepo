package reconcile

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/repository"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	orderv1 "github.com/xiongwp/order-core/api/proto/order/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	reconcileRunTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "accounting_reconcile_run_total",
		Help: "Total number of reconcile worker runs.",
	})
	reconcileRepairTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "accounting_reconcile_repair_total",
		Help: "Total number of ledger repair operations.",
	})
	reconcileDiffCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "accounting_reconcile_diff_count",
		Help: "Number of ledger mismatches detected in latest reconcile run.",
	})
	reconcileScanDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "accounting_reconcile_scan_duration_seconds",
		Help:    "Time taken for one reconcile scan.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	})
)

// ReconcileWorker 跨服务原子性补偿 worker：定时扫描 outbox 失败记录，
// 通过 mTLS gRPC 调 order-core 拿权威状态，对比后修复不一致。
type ReconcileWorker struct {
	outboxRepo repository.SettlementOutboxRepository
	dbManager  *database.Manager
	router     *sharding.Router
	logger     *zap.Logger

	// order-core 客户端（在 Start 时延迟初始化，避免启动期阻塞）
	orderCoreClient orderv1.ChargeServiceClient
	orderConn       *grpc.ClientConn

	// 可配参数
	Interval     time.Duration // 扫描周期（默认 5min）
	StaleThresh  time.Duration // pending 超过多久视为过期（默认 1h）
	grpcEndpoint string        // order-core endpoint（从 etcd service registry 拉）
}

// NewReconcileWorker 构造 worker
func NewReconcileWorker(
	outboxRepo repository.SettlementOutboxRepository,
	dbManager *database.Manager,
	router *sharding.Router,
	logger *zap.Logger,
) *ReconcileWorker {
	return &ReconcileWorker{
		outboxRepo:  outboxRepo,
		dbManager:   dbManager,
		router:      router,
		logger:      logger.Named("reconcile"),
		Interval:    5 * time.Minute,
		StaleThresh: 1 * time.Hour,
	}
}

// SetOrderCoreEndpoint 设置 order-core gRPC endpoint（可由 main.go 或 config 注入）
func (w *ReconcileWorker) SetOrderCoreEndpoint(endpoint string) {
	w.grpcEndpoint = endpoint
}

// Start 启动 worker 的定时扫描循环，直到 ctx 取消。
// 设计上支持 graceful shutdown（ctx.Done() 时优雅退出）。
func (w *ReconcileWorker) Start(ctx context.Context) {
	if w.grpcEndpoint == "" {
		w.logger.Warn("order-core endpoint not configured, reconcile worker skipped")
		return
	}

	// 延迟初始化 gRPC 连接：避免启动期长时间 hang
	if err := w.initOrderCoreClient(); err != nil {
		w.logger.Error("failed to init order-core client", zap.Error(err))
		return
	}
	defer w.closeOrderCoreClient()

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("reconcile worker shutting down")
			return
		case <-ticker.C:
			if err := w.runOnce(ctx); err != nil && ctx.Err() == nil {
				w.logger.Error("reconcile scan failed", zap.Error(err))
			}
		}
	}
}

// runOnce 执行一次扫描：找 outbox 中失败或过期 pending 的记录，通过 gRPC 查权威状态，
// 对比后修复不一致。
func (w *ReconcileWorker) runOnce(ctx context.Context) error {
	start := time.Now()
	defer func() {
		reconcileRunTotal.Inc()
		reconcileScanDuration.Observe(time.Since(start).Seconds())
	}()

	// Step 1: 扫描 outbox 表，找 status=failed 或 status=pending but created_at < now-1h 的记录
	failedRecs, err := w.outboxRepo.FindByStatus(ctx, model.OutboxStatusFailed, 1000)
	if err != nil {
		return fmt.Errorf("find failed outbox records: %w", err)
	}

	staleRecs, err := w.outboxRepo.FindStuckPending(ctx, w.StaleThresh, 1000)
	if err != nil {
		return fmt.Errorf("find stale pending outbox records: %w", err)
	}

	// 合并两个列表
	var allRecs []*model.SettlementOutbox
	allRecs = append(allRecs, failedRecs...)
	allRecs = append(allRecs, staleRecs...)

	if len(allRecs) == 0 {
		w.logger.Debug("reconcile: no failed/stale outbox records found")
		reconcileDiffCount.Set(0)
		return nil
	}

	w.logger.Info("reconcile: scanning records",
		zap.Int("failed", len(failedRecs)),
		zap.Int("stale_pending", len(staleRecs)))

	// Step 2: 对每条记录，解析出 chargeID，调 order-core GetCharge 拿权威状态
	var diffCount int64
	for _, rec := range allRecs {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// 简化：假设 outbox 的 event_data JSON 中含 charge_id 或 payment_intent_id
		// 实际需要根据 accounting-system 的 outbox 模式来解析
		chargeID, piID := w.extractChargePI(rec)
		if chargeID == "" && piID == "" {
			w.logger.Warn("reconcile: outbox record missing charge/pi id",
				zap.String("voucher", rec.VoucherNo))
			continue
		}

		// 调 order-core GetCharge / GetPaymentIntent
		charge, err := w.getChargeFromOrderCore(ctx, chargeID, piID)
		if err != nil {
			w.logger.Warn("reconcile: failed to fetch charge state from order-core",
				zap.String("voucher", rec.VoucherNo),
				zap.String("chargeID", chargeID),
				zap.Error(err))
			continue
		}

		// Step 3: 比对：order-core 说 succeeded 但 accounting 没记 → 调 ledger service 修复
		if charge != nil && charge.Status == orderv1.ChargeStatus_CHARGE_STATUS_SUCCEEDED {
			// 检查 accounting 端是否已记录成功
			if rec.Status != model.OutboxStatusMySQLDone {
				// 修复：写 reconcile_event audit log（目前简化为日志）
				diffCount++
				reconcileRepairTotal.Inc()

				w.logger.Warn("reconcile: ledger mismatch - order-core succeeded but accounting pending",
					zap.String("voucher", rec.VoucherNo),
					zap.String("chargeID", chargeID),
					zap.String("accounting_status", fmt.Sprintf("%d", rec.Status)))

				// TODO: 调用 ledger service 写修复事件（幂等）
				// 目前仅记日志；生产应持久化 reconcile_event audit log
			}
		}
	}

	reconcileDiffCount.Set(float64(diffCount))
	w.logger.Info("reconcile: scan completed",
		zap.Int64("mismatches_found", diffCount),
		zap.Duration("took", time.Since(start)))
	return nil
}

// initOrderCoreClient 初始化 gRPC 客户端连接
func (w *ReconcileWorker) initOrderCoreClient() error {
	if w.orderConn != nil {
		return nil
	}

	// 目前用 insecure（生产应加 mTLS）
	conn, err := grpc.Dial(
		w.grpcEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(10 * 1024 * 1024)),
	)
	if err != nil {
		return fmt.Errorf("dial order-core: %w", err)
	}

	w.orderConn = conn
	w.orderCoreClient = orderv1.NewChargeServiceClient(conn)
	w.logger.Info("order-core gRPC client initialized", zap.String("endpoint", w.grpcEndpoint))
	return nil
}

// closeOrderCoreClient 关闭 gRPC 连接
func (w *ReconcileWorker) closeOrderCoreClient() {
	if w.orderConn != nil {
		_ = w.orderConn.Close()
		w.orderConn = nil
		w.orderCoreClient = nil
	}
}

// getChargeFromOrderCore 调 order-core ChargeService.Retrieve 拿权威 Charge 状态
func (w *ReconcileWorker) getChargeFromOrderCore(ctx context.Context, chargeID, piID string) (*orderv1.Charge, error) {
	if chargeID == "" {
		return nil, fmt.Errorf("empty charge id")
	}

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := w.orderCoreClient.Retrieve(callCtx, &orderv1.RetrieveChargeRequest{Id: chargeID})
	if err != nil {
		return nil, fmt.Errorf("order-core retrieve charge: %w", err)
	}
	return resp.Charge, nil
}

// extractChargePI 从 outbox event_data JSON 中提取 charge_id 和 payment_intent_id
// 简化实现：假设 event_data 是 JSON 字符串，包含 "charge_id" 和 "payment_intent_id" 字段
func (w *ReconcileWorker) extractChargePI(rec *model.SettlementOutbox) (chargeID, piID string) {
	// TODO: 根据实际 outbox event_data 格式解析
	// 目前仅返回空；生产需要具体实现 JSON unmarshal
	return "", ""
}

// Wait 等待 worker 的后台循环退出（用于 graceful shutdown）
func (w *ReconcileWorker) Wait() {
	// worker 通过 ctx.Done() 控制循环，此处为占位
}

// ReconcileEvent audit log 模型（未来扩展：持久化到专用表）
type ReconcileEvent struct {
	ID                int64
	VoucherNo         string
	ChargeID          string
	OrderCoreStatus   string
	AccountingStatus  string
	RepairAction      string
	EventID           string // reconcile_event_id（用于去重幂等）
	CreatedAt         time.Time
}

var (
	repairEventCounter atomic.Int64
)

// RecordReconcileEvent 记录修复事件（幂等）
// 目前仅计数；生产应落 DB 表用于审计和去重
func RecordReconcileEvent(ctx context.Context, event *ReconcileEvent) error {
	repairEventCounter.Add(1)
	// TODO: 写 reconcile_event 表，使用 event_id 作为幂等键（ON DUPLICATE KEY UPDATE）
	return nil
}
