// Package service: 结算业务逻辑骨架。
//
// 当前是 skeleton：定义了 SettlementService 接口 + 一个空实现，让 server / fx
// 装配能跑起来。后续 PR 接 repository（DB 层）+ accounting client，把 Trigger
// 真正实现。
//
// 设计参考 accounting-system day_cut_service：
//   - Trigger() 仅生成新 run，立即返回；实际处理在 goroutine 里
//   - ProcessMerchant() 按商户处理；幂等检查 + 续跑游标
//   - 整体状态机：PENDING → PROCESSING → COMPLETED / FAILED
package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/clearing-settlement/internal/domain"
)

// SettlementService 对外接口。grpc / admin handler 层调用。
type SettlementService interface {
	// TriggerSettlement 启动一次新的结算运行。同 (settleDate, currency) 多次调用
	// 视为重跑（run_id 自增）。**已经 COMPLETED 的 merchant 通过 record 存在
	// 性跳过**，不会重复结算。
	TriggerSettlement(ctx context.Context, settleDate, currency string) (runID int, err error)

	// GetStatus 返回某次运行的聚合状态。runID = 0 → 取该 settleDate 下最大 runID。
	GetStatus(ctx context.Context, settleDate string, runID int) (*RunStatus, error)

	// ResumeStuck 重新派发 PROCESSING 但 updated_at 早于 stuckThreshold 的商户。
	// stuckThreshold = 0 → 强制重派全部 PROCESSING。
	ResumeStuck(ctx context.Context, settleDate string, runID int, stuckThreshold time.Duration) (resumedCount int, err error)
}

// RunStatus 聚合状态用于 admin /admin/settle/status 展示。
type RunStatus struct {
	SettleDate         string `json:"settle_date"`
	RunID              int    `json:"run_id"`
	Currency           string `json:"currency"`
	TotalMerchants     int    `json:"total_merchants"`
	Pending            int    `json:"pending"`
	Processing         int    `json:"processing"`
	Completed          int    `json:"completed"`
	Failed             int    `json:"failed"`
	Skipped            int    `json:"skipped"`
	OverallStatus      domain.SettlementStatus `json:"overall_status"`
}

// settlementService 默认实现骨架。当前所有方法返回 not-implemented；
// 后续 PR 注入 repo + accounting client 后填充逻辑。
type settlementService struct {
	logger *zap.Logger
	mu     sync.Mutex
	// runIDCounter 内存版 run_id 分配器，仅 skeleton 阶段用；接入 DB 后由
	// repo.NextRunID 替代（基于 SELECT MAX(run_id) + 1 with retry on conflict）。
	runIDCounter map[string]int
}

// NewSettlementService 构造默认实现。生产参数（repo / accounting client）
// 后续 PR 通过 fx 注入；当前签名保持简洁便于 e2e。
func NewSettlementService(logger *zap.Logger) SettlementService {
	return &settlementService{
		logger:       logger,
		runIDCounter: make(map[string]int),
	}
}

func (s *settlementService) TriggerSettlement(ctx context.Context, settleDate, currency string) (int, error) {
	if settleDate == "" || currency == "" {
		return 0, fmt.Errorf("settle_date and currency are required")
	}
	s.mu.Lock()
	key := settleDate + "|" + currency
	s.runIDCounter[key]++
	runID := s.runIDCounter[key]
	s.mu.Unlock()

	s.logger.Info("settlement trigger (skeleton no-op)",
		zap.String("settle_date", settleDate),
		zap.String("currency", currency),
		zap.Int("run_id", runID))
	// TODO: 后续 PR
	//   1. repo.UpsertRun(settleDate, runID, currency, PENDING)
	//   2. go process(ctx, settleDate, runID, currency)：扫商户 → 调 accounting → 写 record
	//   3. 完成后 repo.UpdateRunStatus(COMPLETED)
	return runID, nil
}

func (s *settlementService) GetStatus(_ context.Context, settleDate string, runID int) (*RunStatus, error) {
	return &RunStatus{
		SettleDate:    settleDate,
		RunID:         runID,
		OverallStatus: domain.SettlementStatusPending,
	}, nil
}

func (s *settlementService) ResumeStuck(_ context.Context, _ string, _ int, _ time.Duration) (int, error) {
	// 骨架阶段无 PROCESSING merchant 概念
	return 0, nil
}
