package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/idgen"
	"github.com/accounting-system/internal/infrastructure/logging"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/repository"
	"go.uber.org/zap"
)

// AsyncTaskService 异步任务服务接口
type AsyncTaskService interface {
	// CreateTask 创建任务。requestID 必填：作为幂等键，DB 端 uniq_request_id_type
	// 防止重复任务。重复 (taskType, requestID) 命中冲突 → 返回 nil（视作成功，
	// 首次任务仍在跑 / 已完成）。
	CreateTask(ctx context.Context, taskType, requestID, businessNo string, data interface{}, maxRetry int) error

	// ProcessPendingTasks 处理待执行的任务，每次最多处理 batchSize 条（0 表示使用默认值 100）。
	// 返回本次处理的任务总数。
	ProcessPendingTasks(ctx context.Context, batchSize int) (int, error)

	// RetryTask 重试任务
	RetryTask(ctx context.Context, taskID string) error

	// UpdateTaskStatus 更新任务状态
	UpdateTaskStatus(ctx context.Context, taskID string, status int8, errorMsg string) error

	// ListPendingManualTasks 返回超过最大重试次数、需要人工干预的任务列表（最多 limit 条）。
	ListPendingManualTasks(ctx context.Context, limit int) ([]*model.AsyncTask, error)

	// RecoverStuckProcessing 扫描所有分片，将超过 stuckThreshold 仍处于 PROCESSING 状态的任务
	// 重置为 FAILED，使其在下次 ProcessPendingTasks 扫描中被重新处理。
	// 处理服务器突然重启或进程崩溃后任务永远卡在 PROCESSING 的场景（可重入）。
	// 返回本次恢复的任务总数。
	RecoverStuckProcessing(ctx context.Context, stuckThreshold time.Duration) (int64, error)
}

type asyncTaskService struct {
	router        *sharding.Router
	taskRepo      repository.AsyncTaskRepository
	idGen         idgen.IDGenerator
	logger        *zap.Logger // service 层日志（service.log）
	perfLogger    *zap.Logger // 性能日志（performance.log），记录每条任务处理耗时
	handlers      map[string]TaskHandler
	retryPolicies map[string]RetryPolicy
}

// TaskHandler 任务处理器
type TaskHandler func(ctx context.Context, task *model.AsyncTask) error

// RetryPolicy 任务类型级别的重试策略。
// 若未注册，使用任务记录自身的 max_retry_count。
type RetryPolicy struct {
	// MaxRetryCount 该任务类型允许的最大重试次数（覆盖 task.MaxRetryCount）。
	MaxRetryCount int
}

// NewAsyncTaskService 创建异步任务服务
// loggers 用于注入分层日志：
//   - loggers.Service     → service.log（任务生命周期、错误、重试决策）
//   - loggers.Performance → performance.log（每条任务处理耗时）
func NewAsyncTaskService(router *sharding.Router, taskRepo repository.AsyncTaskRepository, idGen idgen.IDGenerator, logger *zap.Logger, loggers *logging.Loggers) AsyncTaskService {
	perfLogger := logger // 降级：若 Loggers 未注入则使用同一个 logger
	if loggers != nil {
		perfLogger = loggers.Performance
	}
	return &asyncTaskService{
		router:        router,
		taskRepo:      taskRepo,
		idGen:         idGen,
		logger:        logger,
		perfLogger:    perfLogger,
		handlers:      make(map[string]TaskHandler),
		retryPolicies: make(map[string]RetryPolicy),
	}
}

// RegisterHandler 注册任务处理器
func (s *asyncTaskService) RegisterHandler(taskType string, handler TaskHandler) {
	s.handlers[taskType] = handler
}

// RegisterRetryPolicy 为指定任务类型注册重试策略，覆盖 task 记录中的 max_retry_count。
// 通常在应用初始化阶段调用，不需要加锁。
func (s *asyncTaskService) RegisterRetryPolicy(taskType string, policy RetryPolicy) {
	s.retryPolicies[taskType] = policy
}

// CreateTask 创建任务（按 businessNo 分库分表）
func (s *asyncTaskService) CreateTask(ctx context.Context, taskType, requestID, businessNo string, data interface{}, maxRetry int) error {
	if requestID == "" {
		return fmt.Errorf("request_id is required for async task idempotency")
	}
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal task data failed: %w", err)
	}

	taskID, err := s.idGen.NextIDStr(ctx, idgen.BizTagAsyncTask)
	if err != nil {
		return fmt.Errorf("generate task id failed: %w", err)
	}

	task := &model.AsyncTask{
		TaskID:        taskID,
		TaskType:      taskType,
		RequestID:     requestID,
		BusinessNo:    businessNo,
		TaskData:      string(dataBytes),
		Status:        model.AsyncTaskStatusPending,
		RetryCount:    0,
		MaxRetryCount: maxRetry,
		NextRetryTime: nil,
	}

	// taskRepo.Create 在底层应实现 ON DUPLICATE KEY UPDATE no-op 或者把
	// MySQL Error 1062 转成 success（首次任务仍在；不算错误）。当前 repo 实现
	// 还没接入这个语义，先 best-effort：返回原 error 让 caller 能日志识别。
	if err := s.taskRepo.Create(ctx, task); err != nil {
		return fmt.Errorf("insert task failed: %w", err)
	}

	s.logger.Info("task created",
		zap.String("taskID", task.TaskID),
		zap.String("taskType", task.TaskType),
		zap.String("requestID", task.RequestID),
		zap.String("businessNo", task.BusinessNo),
	)

	return nil
}

// ProcessPendingTasks 扫描所有分片，处理待执行的任务。
// batchSize 控制每个分片每次最多读取的任务数（0 使用默认值 100）。
// 返回本次成功处理（claimed）的任务总数。
//
// 并发策略：
//   - 分片间：并发查询（goroutine per shard），避免 100 分片串行等待 DB
//   - 分片内：任务并发处理（goroutine per task）；ClaimProcessing 使用 CAS 保证每个任务
//     最多被一个 goroutine 执行，即使多实例同时扫同一分片也安全
func (s *asyncTaskService) ProcessPendingTasks(ctx context.Context, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	now := time.Now()
	shards := s.router.GetAllShards()

	// 1. 并发从所有分片获取待处理任务
	type shardTasks struct {
		tasks []*model.AsyncTask
	}
	shardResults := make([]shardTasks, len(shards))
	{
		var wg sync.WaitGroup
		for i, shard := range shards {
			i, shard := i, shard
			wg.Add(1)
			go func() {
				defer wg.Done()
				tasks, err := s.taskRepo.FindPendingByShard(ctx, shard.DBIndex, shard.TableIndex, now, batchSize)
				if err != nil {
					s.logger.Warn("query tasks failed",
						zap.Int("dbIndex", shard.DBIndex),
						zap.Int("tableIndex", shard.TableIndex),
						zap.Error(err),
					)
					return
				}
				shardResults[i] = shardTasks{tasks: tasks}
			}()
		}
		wg.Wait()
	}

	// 2. 并发处理所有分片的所有任务（CAS 保证多实例安全）
	var (
		wg        sync.WaitGroup
		processed int64
	)
	for _, res := range shardResults {
		for _, task := range res.tasks {
			task := task
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.processTask(ctx, task); err != nil {
					s.logger.Error("process task failed",
						zap.Error(err),
						zap.String("taskID", task.TaskID),
					)
				} else {
					atomic.AddInt64(&processed, 1)
				}
			}()
		}
	}
	wg.Wait()

	return int(processed), nil
}

// processTask 处理单个任务（CAS 争抢 + handler 执行 + 结果更新）
//
// 关键并发安全：
//   - ClaimProcessing 使用 CAS（status = PENDING/FAILED → PROCESSING WHERE status IN (PENDING,FAILED)）
//   - 多实例同时扫同一分片时，只有一个实例能 claim 成功，其余返回 claimed=false 直接跳过
//   - 无需外部锁，数据库行级 CAS 即充当分布式互斥
func (s *asyncTaskService) processTask(ctx context.Context, task *model.AsyncTask) error {
	start := time.Now()

	// CAS: pending/failed → processing（防止多实例重复处理）
	claimed, err := s.taskRepo.ClaimProcessing(ctx, task)
	if err != nil {
		return fmt.Errorf("claim task %s: %w", task.TaskID, err)
	}
	if !claimed {
		return nil // 已被其他实例抢占，无需处理
	}

	handler, exists := s.handlers[task.TaskType]
	if !exists {
		errorMsg := fmt.Sprintf("handler not found for task type: %s", task.TaskType)
		_ = s.taskRepo.MarkPendingManual(ctx, task, errorMsg)
		return fmt.Errorf("%s", errorMsg)
	}

	handlerErr := handler(ctx, task)
	durationMs := float64(time.Since(start).Microseconds()) / 1000.0

	// 写任务处理耗时到 performance.log
	s.perfLogger.Info("task",
		zap.String("taskType", task.TaskType),
		zap.String("taskID", task.TaskID),
		zap.String("businessNo", task.BusinessNo),
		zap.Int("retryCount", task.RetryCount),
		zap.Float64("duration_ms", durationMs),
		zap.Bool("error", handlerErr != nil),
	)

	if handlerErr != nil {
		s.logger.Error("execute task failed",
			zap.Error(handlerErr),
			zap.String("taskID", task.TaskID),
			zap.String("taskType", task.TaskType),
			zap.String("businessNo", task.BusinessNo),
			zap.Int("retryCount", task.RetryCount),
			zap.Int("maxRetryCount", task.MaxRetryCount),
			zap.Float64("duration_ms", durationMs),
		)

		// 使用该 task type 注册的重试策略（若未注册则使用 task 记录自身的 max_retry_count）
		maxRetry := task.MaxRetryCount
		if policy, ok := s.retryPolicies[task.TaskType]; ok {
			maxRetry = policy.MaxRetryCount
		}

		if task.RetryCount < maxRetry {
			return s.taskRepo.ScheduleRetry(ctx, task, handlerErr.Error())
		}
		// 超过最大重试次数 → 进入人工处理流程
		s.logger.Warn("task exceeded max retries, marking for manual processing",
			zap.String("taskID", task.TaskID),
			zap.String("taskType", task.TaskType),
			zap.String("businessNo", task.BusinessNo),
			zap.Int("retryCount", task.RetryCount),
			zap.Int("maxRetryCount", task.MaxRetryCount),
		)
		return s.taskRepo.MarkPendingManual(ctx, task, handlerErr.Error())
	}

	s.logger.Info("task completed successfully",
		zap.String("taskID", task.TaskID),
		zap.String("taskType", task.TaskType),
		zap.String("businessNo", task.BusinessNo),
		zap.Int("retryCount", task.RetryCount),
		zap.Float64("duration_ms", durationMs),
	)
	return s.taskRepo.UpdateSuccess(ctx, task)
}

// RetryTask 重试任务（跨分片查找 taskID）
func (s *asyncTaskService) RetryTask(ctx context.Context, taskID string) error {
	task, err := s.taskRepo.FindByTaskID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("find task %s: %w", taskID, err)
	}
	if task == nil {
		return fmt.Errorf("task not found: %s", taskID)
	}
	return s.processTask(ctx, task)
}

// UpdateTaskStatus 更新任务状态（跨分片查找 taskID）
func (s *asyncTaskService) UpdateTaskStatus(ctx context.Context, taskID string, status int8, errorMsg string) error {
	task, err := s.taskRepo.FindByTaskID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("find task %s: %w", taskID, err)
	}
	if task == nil {
		return fmt.Errorf("task not found: %s", taskID)
	}

	switch status {
	case model.AsyncTaskStatusSuccess:
		return s.taskRepo.UpdateSuccess(ctx, task)
	case model.AsyncTaskStatusFailed:
		return s.taskRepo.UpdateFailed(ctx, task, errorMsg)
	default:
		return fmt.Errorf("unsupported status %d for UpdateTaskStatus", status)
	}
}

// ListPendingManualTasks 返回需要人工干预的任务列表（跨所有分片，最多 limit 条）。
func (s *asyncTaskService) ListPendingManualTasks(ctx context.Context, limit int) ([]*model.AsyncTask, error) {
	if limit <= 0 {
		limit = 50
	}
	var result []*model.AsyncTask
	for _, shard := range s.router.GetAllShards() {
		tasks, err := s.taskRepo.FindPendingManualByShard(ctx, shard.DBIndex, shard.TableIndex, limit)
		if err != nil {
			s.logger.Warn("ListPendingManualTasks: shard query failed",
				zap.Int("dbIndex", shard.DBIndex),
				zap.Int("tableIndex", shard.TableIndex),
				zap.Error(err))
			continue
		}
		result = append(result, tasks...)
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

// RecoverStuckProcessing 将超过 stuckThreshold 仍处于 PROCESSING 的任务重置为 FAILED，
// 使其在下次 ProcessPendingTasks 扫描时被重新拾起。
// 这是"服务器突然重启"场景下的可重入恢复机制：
//   - 服务在 ClaimProcessing（PENDING→PROCESSING）之后、UpdateSuccess 之前崩溃
//   - 任务永远停在 PROCESSING，ProcessPendingTasks 的 FindPendingByShard 不会再捡起它
//   - 定时调用本方法即可将其重置，保证最终一定会被重试（retry_count 已递增，退避仍有效）
func (s *asyncTaskService) RecoverStuckProcessing(ctx context.Context, stuckThreshold time.Duration) (int64, error) {
	recovered, err := s.taskRepo.RecoverStuckProcessing(ctx, stuckThreshold)
	if err != nil {
		return recovered, err
	}
	if recovered > 0 {
		s.logger.Warn("async task recovery: reset stuck PROCESSING tasks to FAILED",
			zap.Int64("count", recovered),
			zap.Duration("stuckThreshold", stuckThreshold),
		)
	}
	return recovered, nil
}
