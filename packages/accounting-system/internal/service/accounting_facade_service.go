package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiongwp/accounting-system/internal/idgen"
	"github.com/xiongwp/accounting-system/internal/infrastructure/kafka"
	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
)

// ExecutionMode 执行模式
type ExecutionMode string

const (
	ExecutionModeSync  ExecutionMode = "SYNC"  // 同步执行
	ExecutionModeAsync ExecutionMode = "ASYNC" // 异步执行
	ExecutionModeBatch ExecutionMode = "BATCH" // 批量执行
)

// BookingRequest 记账请求
type BookingRequest struct {
	Mode           ExecutionMode              `json:"mode"`
	BusinessNo     string                     `json:"business_no"`
	DoubleEntryReq *DoubleEntryBookingRequest `json:"double_entry_req"`
	CallbackURL    string                     `json:"callback_url,omitempty"` // 异步回调地址
}

// BookingResponse 记账响应
type BookingResponse struct {
	Success        bool     `json:"success"`
	RequestID      string   `json:"request_id"`
	VoucherNo      string   `json:"voucher_no,omitempty"`
	TransactionIDs []string `json:"transaction_ids,omitempty"`
	ErrorMessage   string   `json:"error_message,omitempty"`
	Async          bool     `json:"async"` // 是否异步处理
}

// BatchBookingRequest 批量记账请求
type BatchBookingRequest struct {
	Requests []BookingRequest `json:"requests"`
	Parallel bool             `json:"parallel"` // 是否并行处理
}

// BatchBookingResponse 批量记账响应
type BatchBookingResponse struct {
	Success int               `json:"success"`
	Failed  int               `json:"failed"`
	Total   int               `json:"total"`
	Results []BookingResponse `json:"results"`
}

// AccountingFacadeService 记账门面服务
// 提供同步、异步、批量操作的统一接口
type AccountingFacadeService interface {
	// ProcessBooking 处理记账请求（根据mode自动选择同步/异步）
	ProcessBooking(ctx context.Context, req *BookingRequest) (*BookingResponse, error)

	// ProcessBatchBooking 批量处理记账请求
	ProcessBatchBooking(ctx context.Context, req *BatchBookingRequest) (*BatchBookingResponse, error)

	// SyncBooking 同步记账
	SyncBooking(ctx context.Context, req *DoubleEntryBookingRequest) (*BookingResponse, error)

	// AsyncBooking 异步记账（发送到Kafka）
	AsyncBooking(ctx context.Context, req *BookingRequest) (*BookingResponse, error)

	// BatchBooking 批量记账
	BatchBooking(ctx context.Context, requests []*DoubleEntryBookingRequest, parallel bool) (*BatchBookingResponse, error)
}

type accountingFacadeService struct {
	accountingService AccountingService
	asyncTaskService  AsyncTaskService
	kafkaProducer     *kafka.Producer
	idGen             idgen.IDGenerator
	logger            *zap.Logger
}

// NewAccountingFacadeService 创建记账门面服务
func NewAccountingFacadeService(
	accountingService AccountingService,
	asyncTaskService AsyncTaskService,
	kafkaProducer *kafka.Producer,
	idGen idgen.IDGenerator,
	logger *zap.Logger,
) AccountingFacadeService {
	return &accountingFacadeService{
		accountingService: accountingService,
		asyncTaskService:  asyncTaskService,
		kafkaProducer:     kafkaProducer,
		idGen:             idGen,
		logger:            logger,
	}
}

// ProcessBooking 处理记账请求
func (s *accountingFacadeService) ProcessBooking(ctx context.Context, req *BookingRequest) (*BookingResponse, error) {
	switch req.Mode {
	case ExecutionModeSync:
		return s.SyncBooking(ctx, req.DoubleEntryReq)
	case ExecutionModeAsync:
		return s.AsyncBooking(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported execution mode: %s", req.Mode)
	}
}

// SyncBooking 同步记账
func (s *accountingFacadeService) SyncBooking(ctx context.Context, req *DoubleEntryBookingRequest) (*BookingResponse, error) {
	requestID := s.generateRequestID(ctx)

	s.logger.Info("sync booking started",
		zap.String("requestID", requestID),
		zap.String("businessNo", req.BusinessNo),
	)

	// 执行记账
	voucherNo, transactionIDs, err := s.accountingService.DoubleEntryBooking(ctx, req)

	if err != nil {
		s.logger.Error("sync booking failed",
			zap.Error(err),
			zap.String("requestID", requestID),
			zap.String("businessNo", req.BusinessNo),
		)

		return &BookingResponse{
			Success:      false,
			RequestID:    requestID,
			ErrorMessage: err.Error(),
			Async:        false,
		}, nil // 返回业务错误，不返回系统错误
	}

	s.logger.Info("sync booking completed",
		zap.String("requestID", requestID),
		zap.String("voucherNo", voucherNo),
		zap.Strings("transactionIDs", transactionIDs),
	)

	return &BookingResponse{
		Success:        true,
		RequestID:      requestID,
		VoucherNo:      voucherNo,
		TransactionIDs: transactionIDs,
		Async:          false,
	}, nil
}

// AsyncBooking 异步记账
func (s *accountingFacadeService) AsyncBooking(ctx context.Context, req *BookingRequest) (*BookingResponse, error) {
	requestID := s.generateRequestID(ctx)

	s.logger.Info("async booking request received",
		zap.String("requestID", requestID),
		zap.String("businessNo", req.BusinessNo),
	)

	// 创建异步任务
	taskData := map[string]interface{}{
		"request_id":       requestID,
		"double_entry_req": req.DoubleEntryReq,
		"callback_url":     req.CallbackURL,
	}

	if err := s.asyncTaskService.CreateTask(
		ctx,
		"ACCOUNTING",
		requestID,
		req.BusinessNo,
		taskData,
		3, // 最大重试次数
	); err != nil {
		s.logger.Error("create async task failed",
			zap.Error(err),
			zap.String("requestID", requestID),
		)
		return nil, fmt.Errorf("create async task failed: %w", err)
	}

	// 发送到Kafka（Marshal 失败时跳过 Kafka 通知，定时任务重试时会重新发送）
	taskDataBytes, marshalErr := json.Marshal(taskData)
	if marshalErr != nil {
		s.logger.Error("async booking: marshal task data failed, skipping kafka notify",
			zap.Error(marshalErr), zap.String("requestID", requestID))
		taskDataBytes = []byte("{}")
	}
	if err := s.kafkaProducer.SendMessage(
		ctx,
		req.BusinessNo,
		"ACCOUNTING",
		string(taskDataBytes),
	); err != nil {
		s.logger.Error("send kafka message failed",
			zap.Error(err),
			zap.String("requestID", requestID),
		)
		// Kafka发送失败不影响异步任务创建，依靠定时任务重试
	}

	s.logger.Info("async booking task created",
		zap.String("requestID", requestID),
		zap.String("businessNo", req.BusinessNo),
	)

	return &BookingResponse{
		Success:   true,
		RequestID: requestID,
		Async:     true,
	}, nil
}

// ProcessBatchBooking 批量处理记账请求
func (s *accountingFacadeService) ProcessBatchBooking(ctx context.Context, req *BatchBookingRequest) (*BatchBookingResponse, error) {
	s.logger.Info("batch booking started",
		zap.Int("total", len(req.Requests)),
		zap.Bool("parallel", req.Parallel),
	)

	if req.Parallel {
		return s.processBatchBookingParallel(ctx, req.Requests)
	}
	return s.processBatchBookingSequential(ctx, req.Requests)
}

// processBatchBookingParallel 并行处理批量记账
func (s *accountingFacadeService) processBatchBookingParallel(ctx context.Context, requests []BookingRequest) (*BatchBookingResponse, error) {
	results := make([]BookingResponse, len(requests))
	var wg sync.WaitGroup
	var successCount, failedCount atomic.Int64

	for i, req := range requests {
		wg.Add(1)
		go func(index int, request BookingRequest) {
			defer wg.Done()

			response, err := s.ProcessBooking(ctx, &request)
			if err != nil || response == nil || !response.Success {
				failedCount.Add(1)
				if response == nil {
					errMsg := "unknown error"
					if err != nil {
						errMsg = err.Error()
					}
					response = &BookingResponse{
						Success:      false,
						RequestID:    s.generateRequestID(ctx),
						ErrorMessage: errMsg,
					}
				}
			} else {
				successCount.Add(1)
			}
			// Each goroutine writes to a unique index — no mutex needed.
			results[index] = *response
		}(i, req)
	}

	wg.Wait()

	sc := int(successCount.Load())
	fc := int(failedCount.Load())
	s.logger.Info("batch booking completed",
		zap.Int("total", len(requests)),
		zap.Int("success", sc),
		zap.Int("failed", fc),
	)

	return &BatchBookingResponse{
		Success: sc,
		Failed:  fc,
		Total:   len(requests),
		Results: results,
	}, nil
}

// processBatchBookingSequential 顺序处理批量记账
func (s *accountingFacadeService) processBatchBookingSequential(ctx context.Context, requests []BookingRequest) (*BatchBookingResponse, error) {
	results := make([]BookingResponse, len(requests))
	successCount := 0
	failedCount := 0

	for i, req := range requests {
		response, err := s.ProcessBooking(ctx, &req)
		if err != nil || (response != nil && !response.Success) {
			failedCount++
			if response == nil {
				response = &BookingResponse{
					Success:      false,
					RequestID:    s.generateRequestID(ctx),
					ErrorMessage: err.Error(),
				}
			}
		} else {
			successCount++
		}

		results[i] = *response
	}

	s.logger.Info("batch booking completed",
		zap.Int("total", len(requests)),
		zap.Int("success", successCount),
		zap.Int("failed", failedCount),
	)

	return &BatchBookingResponse{
		Success: successCount,
		Failed:  failedCount,
		Total:   len(requests),
		Results: results,
	}, nil
}

// BatchBooking 批量记账（简化接口）
func (s *accountingFacadeService) BatchBooking(ctx context.Context, requests []*DoubleEntryBookingRequest, parallel bool) (*BatchBookingResponse, error) {
	bookingRequests := make([]BookingRequest, len(requests))
	for i, req := range requests {
		bookingRequests[i] = BookingRequest{
			Mode:           ExecutionModeSync,
			BusinessNo:     req.BusinessNo,
			DoubleEntryReq: req,
		}
	}

	return s.ProcessBatchBooking(ctx, &BatchBookingRequest{
		Requests: bookingRequests,
		Parallel: parallel,
	})
}

// generateRequestID 按位编码生成请求 ID（idType=004）。
// fallback：idgen 失败时用 timestamp 当 seq；layout 自身保证 idType 段位独立，
// 不会撞到其他 ID。
func (s *accountingFacadeService) generateRequestID(ctx context.Context) string {
	seq, err := s.idGen.NextID(ctx, idgen.BizTagRequestID)
	if err != nil {
		s.logger.Warn("facade: generate request id failed, fallback to timestamp seq", zap.Error(err))
		// 用 ns 时间戳的低 13 位（idType seq 容量是 13 位 1e13）当兜底 seq；
		// 撞概率极低（同 idType + 同 globalTbl=0 + 同 ns 时间戳低 13 位才撞）。
		seq = time.Now().UnixNano() % 9_999_999_999_999
		if seq == 0 {
			seq = 1
		}
	}
	id, encErr := shadow.EncodeIDStr(ctx, shadow.IDTypeRequestID, 0, seq)
	if encErr != nil {
		s.logger.Warn("facade: encode request id failed", zap.Error(encErr))
		// 极不可能（除非 layout 边界超界）；返回 seq 字符串兜底，不阻断业务。
		return fmt.Sprintf("%d", seq)
	}
	return id
}
