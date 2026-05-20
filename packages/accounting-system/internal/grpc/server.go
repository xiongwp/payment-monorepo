package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	kitexserver "github.com/cloudwego/kitex/server"
	"github.com/xiongwp/accounting-system/internal/currency"
	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/logging"
	"github.com/xiongwp/accounting-system/internal/repository"
	"github.com/xiongwp/accounting-system/internal/service"
	"github.com/shopspring/decimal"
	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	"github.com/xiongwp/payment-util/kitexutil"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountingservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"
	accountingadminservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingadminservice"
	freezeservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/freezeservice"
	transactionservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/transactionservice"
)

// envOrDefault — 单行 env getter, 默认值兜底 (本文件多处使用, 保持代码一致).
func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Server Kitex 服务实现 (同时暴露 4 个 service: AccountingService /
// AccountingAdminService / FreezeService / TransactionService).
// 切 Kitex 后不再 embed Unimplemented*Server (gRPC 兼容性兜底).
type Server struct {
	accountingSvc     service.AccountingService
	transactionSvc    service.TransactionService
	dayCutSvc         service.DayCutService
	tccSvc            service.TccService
	trialBalanceSvc   service.TrialBalanceService
	asyncTaskSvc      service.AsyncTaskService
	freezeSvc         service.FreezeService
	adjustmentSvc     service.AdjustmentService
	transactionRepo   repository.TransactionRepository
	ruleRepo          repository.TransactionRuleRepository // SP-AC-7: ListAccountTypes / ListTransactionRules
	hotAccountRepo    repository.HotAccountRepository
	bufferAccountRepo repository.BufferAccountRepository
	logger            *zap.Logger // API 层日志（api.log）
	perfLogger        *zap.Logger // 性能日志（performance.log），记录每条 RPC 耗时

	// shedder 由 ListenAndServe 启动时赋值；BackpressureController 通过
	// SetMaxInflight 在 outbox 积压时收缩；nil 表示 gRPC server 还没启动。
	shedder *loadShedder

	// serviceToken 服务-到-服务共享 secret，由 main.go 在 ListenAndServe 之前
	// 通过 SetServiceToken 注入。空字符串 = warn-only 模式（dev / 向后兼容）。
	serviceToken string

	// kitexSrv 暴露给 Stop(). ListenAndServe 启动时赋值; nil = 未启动.
	kitexSrv kitexserver.Server
	// done ListenAndServe 退出后关闭; 用作 Stop() 的等待信号.
	done chan struct{}
}

// Stop 优雅关停 Kitex server. SIGTERM 时由 fx OnStop 调用.
// Kitex srv.Stop() 内部已 graceful (等 in-flight RPC 完成); 用 select 限上限.
func (s *Server) Stop(ctx context.Context) error {
	if s.kitexSrv == nil {
		return nil
	}
	doneCh := make(chan struct{})
	go func() {
		_ = s.kitexSrv.Stop()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		return nil
	case <-ctx.Done():
		s.logger.Warn("kitex Stop() timed out")
		return ctx.Err()
	}
}

// SetMaxInflight 运行时调整入口并发上限。0 = 不限。
// nil shedder（gRPC 未启动）安全 no-op。
// 用例：OutboxPendingBackpressure worker 监测 outbox 积压超阈值时收缩。
func (s *Server) SetMaxInflight(n int64) {
	if s.shedder != nil {
		s.shedder.SetMaxInflight(n)
	}
}

// CurrentMaxInflight 当前 max_inflight 上限（监控 / 状态展示用）。
func (s *Server) CurrentMaxInflight() int64 {
	if s.shedder == nil {
		return 0
	}
	return s.shedder.MaxInflight()
}

// NewServer 构造 gRPC Server。
// loggers 提供分层日志实例：
//   - loggers.API         → api.log（请求/响应/耗时）
//   - loggers.Performance → performance.log（耗时数字，供监控告警）
func NewServer(
	accountingSvc service.AccountingService,
	transactionSvc service.TransactionService,
	dayCutSvc service.DayCutService,
	tccSvc service.TccService,
	trialBalanceSvc service.TrialBalanceService,
	asyncTaskSvc service.AsyncTaskService,
	freezeSvc service.FreezeService,
	adjustmentSvc service.AdjustmentService,
	transactionRepo repository.TransactionRepository,
	ruleRepo repository.TransactionRuleRepository,
	hotAccountRepo repository.HotAccountRepository,
	bufferAccountRepo repository.BufferAccountRepository,
	loggers *logging.Loggers,
) *Server {
	return &Server{
		accountingSvc:     accountingSvc,
		transactionSvc:    transactionSvc,
		dayCutSvc:         dayCutSvc,
		tccSvc:            tccSvc,
		trialBalanceSvc:   trialBalanceSvc,
		asyncTaskSvc:      asyncTaskSvc,
		freezeSvc:         freezeSvc,
		adjustmentSvc:     adjustmentSvc,
		transactionRepo:   transactionRepo,
		ruleRepo:          ruleRepo,
		hotAccountRepo:    hotAccountRepo,
		bufferAccountRepo: bufferAccountRepo,
		logger:            loggers.API,
		perfLogger:        loggers.Performance,
	}
}

// ListenAndServe 启动 gRPC 监听（在 goroutine 中运行）
//
// 拦截器链（按 ChainUnary 顺序，从外到内执行）：
//  1. recovery：捕获 handler panic，转成 codes.Internal error，避免整进程崩溃
//  2. timeout（可选）：强制 RPC ctx 上限，防止恶意/超大查询占着 goroutine 不放
//  3. loadshed（可选）：入口 inflight 信号量 + 令牌桶，饱和时 fast-fail 返回
//     ResourceExhausted，打 LoadShedDroppedTotal 计数。保护下游 DB/goroutine/锁。
//  4. logging：记录每条 RPC 的请求体、响应体、耗时到 api.log + performance.log
//
// loadShed 若为空 config（所有阈值 0）则不挂载对应 gate。
// maxRPCDuration = 0 时禁用 timeout 拦截器（开发默认）。
func (s *Server) ListenAndServe(ctx context.Context, port int, loadShed LoadShedConfig, maxRPCDuration time.Duration) error {
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("resolve :%d failed: %w", port, err)
	}

	shedder := newLoadShedder(loadShed)
	shedder.startRefillLoop(ctx, loadShed.RatePerSecond)
	// 暴露给外部 backpressure 控制器（OutboxPendingBackpressure 用），允许运行时
	// 调整 max_inflight 来给热路径限流，避免 OutboxWorker 落库追不上时雪崩。
	s.shedder = shedder

	// 服务-到-服务鉴权状态启动日志（一次）。strict 模式下后续每条非法请求会再单独 log。
	if s.serviceToken == "" {
		s.logger.Warn("gRPC: SERVICE AUTH DISABLED — set ACCOUNTING_SERVICE_TOKEN env var or server.grpc_service_token in config for production")
	} else {
		s.logger.Info("gRPC: service token auth enabled (strict mode)")
	}

	// Kitex MultiService — accounting-system 同时暴露 4 个 service:
	//   AccountingService (业务面 - 落账/查询)
	//   AccountingAdminService (运维面 - 调账/试算/日切)
	//   FreezeService (per-amount 资金冻结)
	//   TransactionService (SP-AC-3 split-payment 主入口)
	//
	// TODO: kitexutil MW (Recover/Trace/Shadow/Timeout/Auth/LoadShed/Logging 7 条)
	// 等 kitexutil port 完成后接 server.WithMiddleware(...).
	//
	// etcd 服务注册: REGISTRY_ENDPOINTS env 非空 → 自动注册到 etcd 为
	// "accounting-service" (docker DNS 名). 上游 caller (split-payment /
	// accounting-admin-web / order-core 等) 用 kitexutil.DefaultClientOptions(
	// "accounting-service") 解析时能立刻找到. ADVERTISE_HOST env 控制广播 host;
	// dev 留空走 container hostname, prod 设 POD_IP.
	serverOpts := []kitexserver.Option{kitexserver.WithServiceAddr(addr)}
	advertise := fmt.Sprintf("%s:%d", envOrDefault("ADVERTISE_HOST", "accounting-service"), port)
	serverOpts = append(serverOpts, kitexutil.DefaultServerOptions("accounting-service", advertise)...)
	srv := kitexserver.NewServer(serverOpts...)
	// ACCT-MULTISVC: multi-service 模式注册 4 个 service. 方法名跨 service 有冲突
	// (e.g. GetTransaction 同时在 AccountingService 和 TransactionService), 必须
	// 给一个 fallback. 选 AccountingService — 它是业务主入口 (落账/查询).
	// (跟 user-merchant-core / order-core 同模式).
	accountingservice.RegisterService(srv, s, kitexserver.WithFallbackService())
	accountingadminservice.RegisterService(srv, s)
	freezeservice.RegisterService(srv, s)
	transactionservice.RegisterService(srv, s)
	// reflection 由 Kitex 内置, 不再手动注册.

	s.kitexSrv = srv
	if s.done == nil {
		s.done = make(chan struct{})
	}

	s.logger.Info("Kitex server listening", zap.Int("port", port), zap.String("addr", addr.String()))

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Run()
		close(s.done)
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("Kitex server shutting down (ctx canceled)")
		_ = srv.Stop()
		<-s.done
		return nil
	case err := <-errCh:
		return err
	}
}

// ─── 账户管理 ─────────────────────────────────────────────────────────────────

func (s *Server) CreateAccount(ctx context.Context, req *accountingv1.CreateAccountRequest) (*accountingv1.CreateAccountResponse, error) {
	if req.Currency == "" {
		req.Currency = "PHP"
	}
	if req.AccountBusinessType == accountingv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_UNSPECIFIED {
		return &accountingv1.CreateAccountResponse{Code: 400, Message: "account_business_type is required"}, nil
	}
	// AccountBusinessType 最大支持 3 位数（1-999）
	if int32(req.AccountBusinessType) > 999 {
		return &accountingv1.CreateAccountResponse{Code: 400, Message: "account_business_type must be between 1 and 999"}, nil
	}
	acc, err := s.accountingSvc.CreateAccount(ctx,
		req.UserId,
		convertAccountBusinessType(req.AccountBusinessType),
		convertAccountType(req.AccountType),
		convertAccountCategory(req.Category),
		req.Currency,
	)
	if err != nil {
		s.logger.Error("CreateAccount failed", zap.Error(err))
		return &accountingv1.CreateAccountResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.CreateAccountResponse{
		Code:    0,
		Message: "ok",
		Account: toProtoAccount(acc),
	}, nil
}

func (s *Server) GetAccount(ctx context.Context, req *accountingv1.GetAccountRequest) (*accountingv1.GetAccountResponse, error) {
	switch id := req.Identifier.(type) {
	case *accountingv1.GetAccountRequest_AccountNo:
		acc, err := s.accountingSvc.GetAccount(ctx, id.AccountNo)
		if err != nil {
			return &accountingv1.GetAccountResponse{Code: 500, Message: err.Error()}, nil
		}
		if acc == nil {
			return &accountingv1.GetAccountResponse{Code: 404, Message: "account not found"}, nil
		}
		return &accountingv1.GetAccountResponse{Code: 0, Message: "ok", Account: toProtoAccount(acc)}, nil
	case *accountingv1.GetAccountRequest_UserIdAndBusinessType:
		q := id.UserIdAndBusinessType
		if q == nil {
			return &accountingv1.GetAccountResponse{Code: 400, Message: "user_id_and_business_type required"}, nil
		}
		acc, err := s.accountingSvc.GetAccountByUserAndBusinessType(ctx, q.UserId, model.AccountBusinessType(q.AccountBusinessType))
		if err != nil {
			return &accountingv1.GetAccountResponse{Code: 500, Message: err.Error()}, nil
		}
		if acc == nil {
			return &accountingv1.GetAccountResponse{Code: 404, Message: "account not found"}, nil
		}
		return &accountingv1.GetAccountResponse{Code: 0, Message: "ok", Account: toProtoAccount(acc)}, nil
	default:
		return &accountingv1.GetAccountResponse{Code: 400, Message: "account_no or user_id_and_business_type required"}, nil
	}
}

// ReloadBufferAccountConfig 从 account_meta.buffer_account_config 重新拉取配置.
// ACCT-MULTISVC: AccountingAdminService interface 要求, 转发给 service 层.
func (s *Server) ReloadBufferAccountConfig(ctx context.Context, _ *accountingv1.ReloadBufferAccountConfigRequest) (*accountingv1.ReloadBufferAccountConfigResponse, error) {
	cnt, err := s.accountingSvc.ReloadBufferAccountConfig(ctx)
	if err != nil {
		s.logger.Warn("ReloadBufferAccountConfig failed", zap.Error(err))
		return &accountingv1.ReloadBufferAccountConfigResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.ReloadBufferAccountConfigResponse{Code: 0, Message: "ok", AccountCount: int32(cnt)}, nil
}

// FreezeAccount 把账户状态从 Active 翻成 Frozen,后续所有出账被拒。
//
// 这是 admin 操作 (风控 / 合规):
//   - 操作前必须有 audit-log + approval-service 双人复核 (网关层校验);
//   - 余额本身不动,FrozenBalance 字段也不动 (那是订单级冻结,独立机制);
//   - 已 in-flight 的 TCC 仍然能 confirm/cancel (按已 leg 上的 lock 走完);
//   - 缓存(BalanceCache) 不需要清,下次读会拿到新 status.
func (s *Server) FreezeAccount(ctx context.Context, req *accountingv1.FreezeAccountRequest) (*accountingv1.FreezeAccountResponse, error) {
	if req == nil || req.AccountNo == "" {
		return &accountingv1.FreezeAccountResponse{Code: 400, Message: "account_no required"}, nil
	}
	if req.Operator == "" {
		return &accountingv1.FreezeAccountResponse{Code: 400, Message: "operator required"}, nil
	}
	if err := s.accountingSvc.SetAccountStatus(ctx, req.AccountNo, model.AccountStatusFrozen, req.Operator, req.Reason); err != nil {
		s.logger.Warn("FreezeAccount failed", zap.String("account_no", req.AccountNo), zap.Error(err))
		return &accountingv1.FreezeAccountResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.FreezeAccountResponse{Code: 0, Message: "ok"}, nil
}

// UnfreezeAccount 把账户状态从 Frozen 翻回 Active。
//
// 同样要求 admin 审批;UpdateBalance / 取现等被禁的接口会立即可用.
func (s *Server) UnfreezeAccount(ctx context.Context, req *accountingv1.UnfreezeAccountRequest) (*accountingv1.UnfreezeAccountResponse, error) {
	if req == nil || req.AccountNo == "" {
		return &accountingv1.UnfreezeAccountResponse{Code: 400, Message: "account_no required"}, nil
	}
	if req.Operator == "" {
		return &accountingv1.UnfreezeAccountResponse{Code: 400, Message: "operator required"}, nil
	}
	if err := s.accountingSvc.SetAccountStatus(ctx, req.AccountNo, model.AccountStatusActive, req.Operator, req.Reason); err != nil {
		s.logger.Warn("UnfreezeAccount failed", zap.String("account_no", req.AccountNo), zap.Error(err))
		return &accountingv1.UnfreezeAccountResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.UnfreezeAccountResponse{Code: 0, Message: "ok"}, nil
}

// ─── 记账操作 ─────────────────────────────────────────────────────────────────

func (s *Server) DoubleEntryBooking(ctx context.Context, req *accountingv1.DoubleEntryBookingRequest) (*accountingv1.DoubleEntryBookingResponse, error) {
	// 从 proto 字段中读取幂等键
	if req.RequestId == "" {
		return &accountingv1.DoubleEntryBookingResponse{
			Code:    400,
			Message: "request_id is required for idempotency",
		}, nil
	}

	if req.Currency == "" {
		req.Currency = "PHP"
	}
	entries := make([]service.AccountingEntry, len(req.Entries))
	for i, e := range req.Entries {
		debit, credit, err := resolveEntryAmounts(e, req.Currency)
		if err != nil {
			return &accountingv1.DoubleEntryBookingResponse{Code: 400, Message: fmt.Sprintf("entry[%d]: %v", i, err)}, nil
		}
		entries[i] = service.AccountingEntry{
			AccountNo:    e.AccountNo,
			DebitAmount:  debit,
			CreditAmount: credit,
			Description:  e.Description,
		}
	}
	voucherNo, txIDs, err := s.accountingSvc.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
		RequestID:    req.RequestId,
		BusinessNo:   req.BusinessNo,
		BusinessType: convertBusinessType(req.BusinessType),
		Entries:      entries,
		Currency:     req.Currency,
		Description:  req.Description,
	})
	if err != nil {
		if errors.Is(err, service.ErrRequestInProgress) {
			return &accountingv1.DoubleEntryBookingResponse{Code: 409, Message: err.Error()}, nil
		}
		s.logger.Error("DoubleEntryBooking failed", zap.Error(err))
		return &accountingv1.DoubleEntryBookingResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.DoubleEntryBookingResponse{
		Code:           0,
		Message:        "ok",
		VoucherNo:      voucherNo,
		TransactionIds: txIDs,
	}, nil
}

func (s *Server) BatchBooking(ctx context.Context, req *accountingv1.BatchBookingRequest) (*accountingv1.BatchBookingResponse, error) {
	results := make([]*accountingv1.DoubleEntryBookingResponse, 0, len(req.Requests))
	var success, failed int32
	for _, r := range req.Requests {
		res, err := s.DoubleEntryBooking(ctx, r)
		if err != nil {
			res = &accountingv1.DoubleEntryBookingResponse{Code: 500, Message: err.Error()}
		}
		results = append(results, res)
		if res.Code == 0 {
			success++
		} else {
			failed++
		}
	}
	return &accountingv1.BatchBookingResponse{
		Code:    0,
		Message: "ok",
		Success: success,
		Failed:  failed,
		Total:   int32(len(req.Requests)),
		Results: results,
	}, nil
}

func (s *Server) MoneyFlow(ctx context.Context, req *accountingv1.MoneyFlowRequest) (*accountingv1.MoneyFlowResponse, error) {
	return &accountingv1.MoneyFlowResponse{Code: 501, Message: "not implemented"}, nil
}

// HybridDoubleEntryBooking 混合记账（热路径/冷路径自动路由 + 预生成 ID + 参数持久化）
func (s *Server) HybridDoubleEntryBooking(ctx context.Context, req *accountingv1.HybridDoubleEntryBookingRequest) (*accountingv1.HybridDoubleEntryBookingResponse, error) {
	if req.RequestId == "" {
		return &accountingv1.HybridDoubleEntryBookingResponse{Code: 400, Message: "request_id is required"}, nil
	}

	if req.Currency == "" {
		req.Currency = "PHP"
	}
	entries := make([]service.AccountingEntry, len(req.Entries))
	for i, e := range req.Entries {
		debit, credit, err := resolveEntryAmounts(e, req.Currency)
		if err != nil {
			return &accountingv1.HybridDoubleEntryBookingResponse{Code: 400, Message: fmt.Sprintf("entry[%d]: %v", i, err)}, nil
		}
		entries[i] = service.AccountingEntry{
			AccountNo:    e.AccountNo,
			DebitAmount:  debit,
			CreditAmount: credit,
			Description:  e.Description,
		}
	}
	svcReq := &service.DoubleEntryBookingRequest{
		RequestID:    req.RequestId,
		BusinessNo:   req.BusinessNo,
		BusinessType: convertBusinessType(req.BusinessType),
		Entries:      entries,
		Currency:     req.Currency,
		Description:  req.Description,
	}

	voucherNo, txIDs, idempotentHit, err := s.accountingSvc.HybridDoubleEntryBooking(ctx, svcReq)
	if err != nil {
		if errors.Is(err, service.ErrRequestInProgress) {
			return &accountingv1.HybridDoubleEntryBookingResponse{Code: 409, Message: err.Error()}, nil
		}
		s.logger.Error("HybridDoubleEntryBooking failed", zap.Error(err))
		return &accountingv1.HybridDoubleEntryBookingResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.HybridDoubleEntryBookingResponse{
		Code:           0,
		Message:        "ok",
		VoucherNo:      voucherNo,
		TransactionIds: txIDs,
		IdempotentHit:  idempotentHit,
	}, nil
}

// AtomicBatchBooking 原子批量记账（全部成功 or 全部回滚，先持久化再执行）
func (s *Server) AtomicBatchBooking(ctx context.Context, req *accountingv1.AtomicBatchBookingRequest) (*accountingv1.AtomicBatchBookingResponse, error) {
	if req.BatchRequestId == "" {
		return &accountingv1.AtomicBatchBookingResponse{Code: 400, Message: "batch_request_id is required"}, nil
	}
	if len(req.Requests) == 0 {
		return &accountingv1.AtomicBatchBookingResponse{Code: 400, Message: "requests cannot be empty"}, nil
	}
	svcRequests := make([]service.DoubleEntryBookingRequest, len(req.Requests))
	for i, r := range req.Requests {
		entries := make([]service.AccountingEntry, len(r.Entries))
		cur := r.Currency
		if cur == "" {
			cur = "PHP"
		}
		for j, e := range r.Entries {
			debit, credit, err := resolveEntryAmounts(e, cur)
			if err != nil {
				return &accountingv1.AtomicBatchBookingResponse{Code: 400, Message: fmt.Sprintf("request[%d] entry[%d]: %v", i, j, err)}, nil
			}
			entries[j] = service.AccountingEntry{
				AccountNo:    e.AccountNo,
				DebitAmount:  debit,
				CreditAmount: credit,
				Description:  e.Description,
			}
		}
		svcRequests[i] = service.DoubleEntryBookingRequest{
			RequestID:    r.RequestId,
			BusinessNo:   r.BusinessNo,
			BusinessType: convertBusinessType(r.BusinessType),
			Entries:      entries,
			Currency:     cur,
			Description:  r.Description,
		}
	}

	result, err := s.accountingSvc.AtomicBatchBooking(ctx, &service.AtomicBatchBookingRequest{
		BatchRequestID:  req.BatchRequestId,
		BatchBusinessNo: req.BatchBusinessNo,
		Requests:        svcRequests,
		Description:     req.Description,
	})
	if err != nil {
		s.logger.Error("AtomicBatchBooking failed", zap.Error(err),
			zap.String("batchRequestID", req.BatchRequestId))
		code := int32(500)
		if result != nil {
			code = 422 // unprocessable: batch tried but failed, rollback done
		}
		msg := err.Error()
		var protoResults []*accountingv1.AtomicBatchBookingItemResult
		if result != nil {
			protoResults = toProtoAtomicBatchItems(result.Items)
		}
		return &accountingv1.AtomicBatchBookingResponse{
			Code:       code,
			Message:    msg,
			BatchId:    req.BatchRequestId,
			AllSuccess: false,
			Total:      int32(len(req.Requests)),
			Results:    protoResults,
		}, nil
	}
	return &accountingv1.AtomicBatchBookingResponse{
		Code:       0,
		Message:    "ok",
		BatchId:    result.BatchID,
		AllSuccess: result.AllSuccess,
		Total:      int32(len(result.Items)),
		Results:    toProtoAtomicBatchItems(result.Items),
	}, nil
}

func toProtoAtomicBatchItems(items []service.BatchBookingItemResult) []*accountingv1.AtomicBatchBookingItemResult {
	out := make([]*accountingv1.AtomicBatchBookingItemResult, len(items))
	for i, it := range items {
		r := &accountingv1.AtomicBatchBookingItemResult{
			RequestId:      it.RequestID,
			VoucherNo:      it.VoucherNo,
			TransactionIds: it.TxIDs,
			Code:           0,
		}
		if it.Err != nil {
			r.Code = 500
			r.ErrorMessage = it.Err.Error()
		}
		out[i] = r
	}
	return out
}

// ─── 查询操作 ─────────────────────────────────────────────────────────────────

func (s *Server) GetTransaction(ctx context.Context, req *accountingv1.GetTransactionRequest) (*accountingv1.GetTransactionResponse, error) {
	pageSize := int(req.PageSize)
	if pageSize <= 0 {
		pageSize = 30
	}
	pageNum := int(req.PageNum)
	if pageNum <= 0 {
		pageNum = 1
	}
	offset := (pageNum - 1) * pageSize

	rows, total, err := s.transactionRepo.ListTransactions(ctx, repository.ListTransactionFilter{
		AccountNo:     req.AccountNo,
		BusinessNo:    req.BusinessNo,
		TransactionID: req.TransactionId,
		StartDate:     req.StartDate,
		EndDate:       req.EndDate,
		Offset:        offset,
		Limit:         pageSize,
	})
	if err != nil {
		return &accountingv1.GetTransactionResponse{Code: 500, Message: err.Error()}, nil
	}

	out := make([]*accountingv1.Transaction, len(rows))
	for i, t := range rows {
		debit, _ := currency.FormatAmount(t.DebitAmount, t.Currency)
		credit, _ := currency.FormatAmount(t.CreditAmount, t.Currency)
		balBefore, _ := currency.FormatAmount(t.BalanceBefore, t.Currency)
		balAfter, _ := currency.FormatAmount(t.BalanceAfter, t.Currency)
		proto := &accountingv1.Transaction{
			TransactionId:   t.TransactionID,
			AccountNo:       t.AccountNo,
			BusinessNo:      t.BusinessNo,
			BusinessType:    convertBusinessTypeToProto(t.BusinessType),
			DebitAmount:     debit,
			CreditAmount:    credit,
			BalanceBefore:   balBefore,
			BalanceAfter:    balAfter,
			Currency:        t.Currency,
			TransactionDate: t.TransactionDate,
			TransactionTime: timestamppb.New(t.TransactionTime),
			Status:          int32(t.Status),
			BookingType:     int32(t.BookingType),
		}
		if t.ParentTransactionID != nil {
			proto.ParentTransactionId = *t.ParentTransactionID
		}
		if t.Description != nil {
			proto.Description = *t.Description
		}
		out[i] = proto
	}

	return &accountingv1.GetTransactionResponse{
		Code:         0,
		Message:      "ok",
		Transactions: out,
		Total:        int32(total),
	}, nil
}

func (s *Server) GetBalanceSnapshot(ctx context.Context, req *accountingv1.GetBalanceSnapshotRequest) (*accountingv1.GetBalanceSnapshotResponse, error) {
	snap, err := s.accountingSvc.GetBalanceSnapshot(ctx, req.AccountNo, req.SnapshotDate)
	if err != nil {
		return &accountingv1.GetBalanceSnapshotResponse{Code: 500, Message: err.Error()}, nil
	}
	if snap == nil {
		return &accountingv1.GetBalanceSnapshotResponse{Code: 404, Message: "snapshot not found"}, nil
	}
	beginBal, _ := currency.FormatAmount(snap.BeginningBalance, snap.Currency)
	endBal, _ := currency.FormatAmount(snap.EndingBalance, snap.Currency)
	totDebit, _ := currency.FormatAmount(snap.TotalDebit, snap.Currency)
	totCredit, _ := currency.FormatAmount(snap.TotalCredit, snap.Currency)
	return &accountingv1.GetBalanceSnapshotResponse{
		Code:    0,
		Message: "ok",
		Snapshot: &accountingv1.BalanceSnapshot{
			AccountNo:        snap.AccountNo,
			SnapshotDate:     snap.SnapshotDate,
			BeginningBalance: beginBal,
			EndingBalance:    endBal,
			TotalDebit:       totDebit,
			TotalCredit:      totCredit,
			TransactionCount: int32(snap.TransactionCount),
		},
	}, nil
}

// ─── 管理操作 ─────────────────────────────────────────────────────────────────

func (s *Server) TriggerDayCut(ctx context.Context, req *accountingv1.TriggerDayCutRequest) (*accountingv1.TriggerDayCutResponse, error) {
	if req.Currency == "" {
		return &accountingv1.TriggerDayCutResponse{Code: 400, Message: "currency 必填：日切按币种独立执行"}, nil
	}
	if err := s.dayCutSvc.TriggerDayCut(ctx, req.CutDate, req.Currency); err != nil {
		s.logger.Error("TriggerDayCut failed", zap.Error(err))
		return &accountingv1.TriggerDayCutResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.TriggerDayCutResponse{Code: 0, Message: "ok"}, nil
}

func (s *Server) RunTrialBalance(ctx context.Context, req *accountingv1.RunTrialBalanceRequest) (*accountingv1.RunTrialBalanceResponse, error) {
	if req.SnapshotDate == "" {
		return &accountingv1.RunTrialBalanceResponse{Code: 400, Message: "snapshot_date is required"}, nil
	}
	if req.Currency == "" {
		return &accountingv1.RunTrialBalanceResponse{
			Code:    400,
			Message: "currency 必填：试算平衡按币种独立执行",
		}, nil
	}

	// 日切未全部完成时拒绝执行试算平衡，避免使用残缺快照产生误导性结果。
	runID, found, err := s.dayCutSvc.GetLatestCompletedRunID(ctx, req.SnapshotDate)
	if err != nil {
		s.logger.Error("RunTrialBalance: check day cut status failed", zap.Error(err))
		return &accountingv1.RunTrialBalanceResponse{Code: 500, Message: "检查日切状态失败: " + err.Error()}, nil
	}
	if !found {
		return &accountingv1.RunTrialBalanceResponse{
			Code:    400,
			Message: fmt.Sprintf("日切尚未完成，无法执行试算平衡（日期 %s 尚无全部分片完成的日切记录）", req.SnapshotDate),
		}, nil
	}

	result, err := s.trialBalanceSvc.RunTrialBalanceByCurrency(ctx, req.SnapshotDate, req.Currency, runID)
	if err != nil {
		// Partial results still get returned with a warning code.
		s.logger.Warn("RunTrialBalance completed with errors", zap.Error(err))
		if result == nil {
			return &accountingv1.RunTrialBalanceResponse{Code: 500, Message: err.Error()}, nil
		}
		// 206: partial content — some shards failed but we have data.
		return toProtoTrialBalanceResponse(206, err.Error(), result), nil
	}
	return toProtoTrialBalanceResponse(0, "ok", result), nil
}

func toProtoTrialBalanceResponse(code int32, msg string, r *service.TrialBalanceResult) *accountingv1.RunTrialBalanceResponse {
	summaries := make([]*accountingv1.TrialBalanceCategorySummary, len(r.Summaries))
	for i, s := range r.Summaries {
		summaries[i] = &accountingv1.TrialBalanceCategorySummary{
			Category:     string(s.Category),
			Type:         int32(s.Type),
			AccountCount: s.AccountCount,
			SumBeginning: strconv.FormatInt(s.SumBeginning, 10),
			SumEnding:    strconv.FormatInt(s.SumEnding, 10),
			SumDebit:     strconv.FormatInt(s.SumDebit, 10),
			SumCredit:    strconv.FormatInt(s.SumCredit, 10),
		}
	}
	return &accountingv1.RunTrialBalanceResponse{
		Code:                   code,
		Message:                msg,
		SnapshotDate:           r.SnapshotDate,
		Summaries:              summaries,
		TotalDebit:             strconv.FormatInt(r.TotalDebit, 10),
		TotalCredit:            strconv.FormatInt(r.TotalCredit, 10),
		IsBalanced:             r.IsBalanced,
		Imbalance:              strconv.FormatInt(r.Imbalance, 10),
		AssetEndingBalance:     strconv.FormatInt(r.AssetEndingBalance, 10),
		LiabilityEndingBalance: strconv.FormatInt(r.LiabilityEndingBalance, 10),
		EquityEndingBalance:    strconv.FormatInt(r.EquityEndingBalance, 10),
		RevenueEndingBalance:   strconv.FormatInt(r.RevenueEndingBalance, 10),
		ExpenseEndingBalance:   strconv.FormatInt(r.ExpenseEndingBalance, 10),
		IsEquationValid:        r.IsEquationValid,
		EquationDiff:           strconv.FormatInt(r.EquationDiff, 10),
	}
}

// AdjustBalance 调账（双分录 + TCC + Plan B 切窗）。
//
// 入参解析：amount 优先取 amount_money（推荐），缺失时回落老 amount string（major units）。
// currency 从请求字段读，缺省尝试从 amount_money.currency。
// request_id 优先从 metadata x-request-id（与其他 booking 入口一致），其次取请求字段。
func (s *Server) AdjustBalance(ctx context.Context, req *accountingv1.AdjustBalanceRequest) (*accountingv1.AdjustBalanceResponse, error) {
	if req == nil {
		return &accountingv1.AdjustBalanceResponse{Code: 400, Message: "nil request"}, nil
	}

	// 币种解析（amount_money 优先于请求顶层 currency）。
	// admin-web 调账场景客户端不传 currency —— 账户表已经带 currency 字段，
	// 这里向 target 账户兜底。
	currencyCode := req.GetCurrency()
	if m := req.GetAmountMoney(); m != nil && m.GetCurrency() != "" {
		currencyCode = m.GetCurrency()
	}
	if currencyCode == "" {
		if req.GetAccountNo() == "" {
			return &accountingv1.AdjustBalanceResponse{Code: 400, Message: "account_no is required (cannot derive currency)"}, nil
		}
		acc, gerr := s.accountingSvc.GetAccount(ctx, req.GetAccountNo())
		if gerr != nil {
			return &accountingv1.AdjustBalanceResponse{Code: 500, Message: fmt.Sprintf("get target account: %v", gerr)}, nil
		}
		if acc == nil {
			return &accountingv1.AdjustBalanceResponse{Code: 404, Message: fmt.Sprintf("account not found: %s", req.GetAccountNo())}, nil
		}
		currencyCode = acc.Currency
	}

	// 金额解析：amount_money 优先（minor_units → storage），否则老 amount（decimal major → storage）
	var amount int64
	if m := req.GetAmountMoney(); m != nil {
		v, err := currency.ToStorageFromMinor(m.GetMinorUnits(), currencyCode)
		if err != nil {
			return &accountingv1.AdjustBalanceResponse{Code: 400, Message: fmt.Sprintf("amount_money: %v", err)}, nil
		}
		amount = v
	} else {
		dec, err := decimal.NewFromString(req.GetAmount())
		if err != nil {
			return &accountingv1.AdjustBalanceResponse{Code: 400, Message: fmt.Sprintf("invalid amount %q: %v", req.GetAmount(), err)}, nil
		}
		v, err := currency.ToStorage(dec, currencyCode)
		if err != nil {
			return &accountingv1.AdjustBalanceResponse{Code: 400, Message: fmt.Sprintf("amount: %v", err)}, nil
		}
		amount = v
	}

	// request_id: 老 gRPC metadata 路径已删, Kitex MW 接通后从 metainfo 拿 x-request-id 覆盖.
	requestID := req.GetRequestId()

	resp, err := s.adjustmentSvc.AdjustBalance(ctx, &service.AdjustmentRequest{
		AccountNo:       req.GetAccountNo(),
		OffsetAccountNo: req.GetOffsetAccountNo(),
		AdjustmentType:  service.AdjustmentType(req.GetAdjustmentType()),
		Amount:          amount,
		IsIncrease:      req.GetIsIncrease(),
		Currency:        currencyCode,
		Reason:          req.GetReason(),
		Operator:        req.GetOperator(),
		ApprovalNo:      req.GetApprovalNo(),
		RelatedTxID:     req.GetRelatedTxId(),
		RequestID:       requestID,
	})
	if err != nil {
		s.logger.Error("AdjustBalance: internal error",
			zap.String("accountNo", req.GetAccountNo()),
			zap.String("requestID", requestID),
			zap.Error(err))
		return &accountingv1.AdjustBalanceResponse{Code: 500, Message: err.Error()}, nil
	}
	if !resp.Success {
		return &accountingv1.AdjustBalanceResponse{Code: 400, Message: resp.ErrorMessage}, nil
	}
	return &accountingv1.AdjustBalanceResponse{
		Code:          0,
		Message:       "OK",
		TransactionId: resp.TransactionID,
		VoucherNo:     resp.VoucherNo,
		BalanceBefore: strconv.FormatInt(resp.BalanceBefore, 10),
		BalanceAfter:  strconv.FormatInt(resp.BalanceAfter, 10),
	}, nil
}

// ─── TCC 维护 ─────────────────────────────────────────────────────────────────

func (s *Server) GetTccStatus(ctx context.Context, req *accountingv1.GetTccStatusRequest) (*accountingv1.GetTccStatusResponse, error) {
	branches, err := s.tccSvc.GetTccStatus(ctx, req.TccId)
	if err != nil {
		return &accountingv1.GetTccStatusResponse{Code: 500, Message: err.Error()}, nil
	}
	if len(branches) == 0 {
		return &accountingv1.GetTccStatusResponse{Code: 404, Message: "tcc not found", TccId: req.TccId}, nil
	}

	// 计算整体状态
	var tryingCount, confirmedCount, cancelledCount int
	for _, b := range branches {
		switch b.Status {
		case model.TccStatusTrying:
			tryingCount++
		case model.TccStatusConfirmed:
			confirmedCount++
		case model.TccStatusCancelled:
			cancelledCount++
		}
	}
	var overallStatus string
	switch {
	case tryingCount == 0 && cancelledCount == 0:
		overallStatus = "CONFIRMED"
	case tryingCount == 0 && confirmedCount == 0:
		overallStatus = "CANCELLED"
	case tryingCount > 0 && confirmedCount == 0 && cancelledCount == 0:
		overallStatus = "TRYING"
	default:
		overallStatus = "PARTIAL"
	}

	protoBranches := make([]*accountingv1.TccBranchInfo, len(branches))
	for i, b := range branches {
		protoBranches[i] = toProtoTccBranch(b)
	}
	return &accountingv1.GetTccStatusResponse{
		Code:          0,
		Message:       "ok",
		TccId:         req.TccId,
		OverallStatus: overallStatus,
		BranchCount:   int32(len(branches)),
		Branches:      protoBranches,
	}, nil
}

func (s *Server) ListStuckTcc(ctx context.Context, req *accountingv1.ListStuckTccRequest) (*accountingv1.ListStuckTccResponse, error) {
	branches, err := s.tccSvc.ListStuck(ctx, int(req.TimeoutMinutes), int(req.Limit))
	if err != nil {
		return &accountingv1.ListStuckTccResponse{Code: 500, Message: err.Error()}, nil
	}
	protoBranches := make([]*accountingv1.TccBranchInfo, len(branches))
	for i, b := range branches {
		protoBranches[i] = toProtoTccBranch(b)
	}
	return &accountingv1.ListStuckTccResponse{
		Code:     0,
		Message:  "ok",
		Count:    int32(len(branches)),
		Branches: protoBranches,
	}, nil
}

func (s *Server) CancelTcc(ctx context.Context, req *accountingv1.CancelTccRequest) (*accountingv1.CancelTccResponse, error) {
	if err := s.tccSvc.CancelTcc(ctx, req.TccId); err != nil {
		return &accountingv1.CancelTccResponse{Code: 500, Message: err.Error(), TccId: req.TccId, Result: "FAILED"}, nil
	}
	return &accountingv1.CancelTccResponse{Code: 0, Message: "ok", TccId: req.TccId, Result: "CANCELLED"}, nil
}

func (s *Server) CancelTccBranch(ctx context.Context, req *accountingv1.CancelTccBranchRequest) (*accountingv1.CancelTccBranchResponse, error) {
	if err := s.tccSvc.CancelBranch(ctx, req.BranchId, req.AccountNo); err != nil {
		return &accountingv1.CancelTccBranchResponse{Code: 500, Message: err.Error(), BranchId: req.BranchId, Result: "FAILED"}, nil
	}
	return &accountingv1.CancelTccBranchResponse{Code: 0, Message: "ok", BranchId: req.BranchId, Result: "CANCELLED"}, nil
}

// ─── 管理查询 ─────────────────────────────────────────────────────────────────
//
// ListDayCutHistory / ListSnapshotDates / RebuildHotAccounts /
// ListAccountsByUserAndBusinessType 4 个 handler 已删 — 它们的 Request/Response
// proto 类型从未声明在 accounting.proto 里. 要恢复需要先把这 4 个 RPC 的
// message types + service 声明加进 .proto, 再重新生成 kitex_gen, 然后把 handler
// 加回来. admin-web 调这些 RPC 的功能临时不可用.

// ─── AccountingAdminService 实现 ──────────────────────────────────────────────

// ProcessAsyncTasks 处理待执行的异步任务（批量，每次最多 batchSize 条）。
func (s *Server) ProcessAsyncTasks(ctx context.Context, req *accountingv1.ProcessAsyncTasksRequest) (*accountingv1.ProcessAsyncTasksResponse, error) {
	batchSize := int(req.BatchSize)
	processed, err := s.asyncTaskSvc.ProcessPendingTasks(ctx, batchSize)
	if err != nil {
		s.logger.Error("ProcessAsyncTasks failed", zap.Error(err))
		return &accountingv1.ProcessAsyncTasksResponse{Code: 500, Message: err.Error()}, nil
	}
	s.logger.Info("ProcessAsyncTasks completed", zap.Int("processed", processed))
	return &accountingv1.ProcessAsyncTasksResponse{
		Code:      0,
		Message:   "ok",
		Processed: int32(processed),
	}, nil
}

// DayCutWatchdog 扫描当天所有日切分片，对超时卡住的分片自动重跑。
func (s *Server) DayCutWatchdog(ctx context.Context, req *accountingv1.DayCutWatchdogRequest) (*accountingv1.DayCutWatchdogResponse, error) {
	thresholdSec := req.StuckThresholdSeconds
	if thresholdSec <= 0 {
		thresholdSec = 300
	}
	threshold := time.Duration(thresholdSec) * time.Second

	if err := s.dayCutSvc.WatchdogRecover(ctx, threshold); err != nil {
		s.logger.Error("DayCutWatchdog failed", zap.Error(err))
		return &accountingv1.DayCutWatchdogResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.DayCutWatchdogResponse{Code: 0, Message: "ok"}, nil
}

// ListManualTasks 返回超过最大重试次数、需要人工干预的任务列表。
func (s *Server) ListManualTasks(ctx context.Context, req *accountingv1.ListManualTasksRequest) (*accountingv1.ListManualTasksResponse, error) {
	limit := int(req.Limit)
	tasks, err := s.asyncTaskSvc.ListPendingManualTasks(ctx, limit)
	if err != nil {
		s.logger.Error("ListManualTasks failed", zap.Error(err))
		return &accountingv1.ListManualTasksResponse{Code: 500, Message: err.Error()}, nil
	}

	protoTasks := make([]*accountingv1.ManualTask, 0, len(tasks))
	for _, t := range tasks {
		errMsg := ""
		if t.ErrorMessage != nil {
			errMsg = *t.ErrorMessage
		}
		protoTasks = append(protoTasks, &accountingv1.ManualTask{
			TaskId:       t.TaskID,
			TaskType:     t.TaskType,
			BusinessNo:   t.BusinessNo,
			ErrorMessage: errMsg,
			RetryCount:   int32(t.RetryCount),
		})
	}
	return &accountingv1.ListManualTasksResponse{Code: 0, Message: "ok", Tasks: protoTasks}, nil
}

// RecoverStuckTasks 将超过阈值仍处于 PROCESSING 的异步任务重置为 FAILED，使其可被重试。
// 处理服务器突然重启或进程崩溃后任务永久卡住的场景（可重入）。
func (s *Server) RecoverStuckTasks(ctx context.Context, req *accountingv1.RecoverStuckTasksRequest) (*accountingv1.RecoverStuckTasksResponse, error) {
	thresholdSec := req.StuckThresholdSeconds
	if thresholdSec <= 0 {
		thresholdSec = 300
	}
	threshold := time.Duration(thresholdSec) * time.Second

	recovered, err := s.asyncTaskSvc.RecoverStuckProcessing(ctx, threshold)
	if err != nil {
		s.logger.Error("RecoverStuckTasks failed", zap.Error(err))
		return &accountingv1.RecoverStuckTasksResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.RecoverStuckTasksResponse{
		Code:      0,
		Message:   "ok",
		Recovered: recovered,
	}, nil
}

// ─── 缓冲记账账户 CRUD ────────────────────────────────────────────────────────

func (s *Server) ListBufferAccounts(ctx context.Context, _ *accountingv1.ListBufferAccountsRequest) (*accountingv1.ListBufferAccountsResponse, error) {
	list, err := s.bufferAccountRepo.ListAll(ctx)
	if err != nil {
		return &accountingv1.ListBufferAccountsResponse{Code: 500, Message: err.Error()}, nil
	}
	items := make([]*accountingv1.BufferAccountEntry, 0, len(list))
	for _, cfg := range list {
		items = append(items, &accountingv1.BufferAccountEntry{
			Id:                 cfg.ID,
			AccountNo:          cfg.AccountNo,
			FlushIntervalLevel: int32(cfg.FlushIntervalLevel),
			Enabled:            cfg.Enabled,
			Description:        cfg.Description,
			CreatedAt:          cfg.CreatedAt.Format(time.RFC3339),
			UpdatedAt:          cfg.UpdatedAt.Format(time.RFC3339),
		})
	}
	return &accountingv1.ListBufferAccountsResponse{Code: 0, Message: "ok", Items: items}, nil
}

func (s *Server) CreateBufferAccount(ctx context.Context, req *accountingv1.CreateBufferAccountRequest) (*accountingv1.CreateBufferAccountResponse, error) {
	if req.AccountNo == "" {
		return &accountingv1.CreateBufferAccountResponse{Code: 400, Message: "account_no is required"}, nil
	}
	validLevels := map[int32]bool{1: true, 5: true, 10: true, 60: true, 1440: true}
	if !validLevels[req.FlushIntervalLevel] {
		return &accountingv1.CreateBufferAccountResponse{Code: 400, Message: "flush_interval_level must be one of: 1, 5, 10, 60, 1440"}, nil
	}
	cfg := &model.BufferAccountConfig{
		AccountNo:          req.AccountNo,
		FlushIntervalLevel: model.BufferFlushLevel(req.FlushIntervalLevel),
		Enabled:            true,
		Description:        req.Description,
	}
	if err := s.bufferAccountRepo.Create(ctx, cfg); err != nil {
		return &accountingv1.CreateBufferAccountResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.CreateBufferAccountResponse{
		Code:    0,
		Message: "ok",
		Item: &accountingv1.BufferAccountEntry{
			Id:                 cfg.ID,
			AccountNo:          cfg.AccountNo,
			FlushIntervalLevel: int32(cfg.FlushIntervalLevel),
			Enabled:            cfg.Enabled,
			Description:        cfg.Description,
			CreatedAt:          cfg.CreatedAt.Format(time.RFC3339),
			UpdatedAt:          cfg.UpdatedAt.Format(time.RFC3339),
		},
	}, nil
}

func (s *Server) UpdateBufferAccount(ctx context.Context, req *accountingv1.UpdateBufferAccountRequest) (*accountingv1.UpdateBufferAccountResponse, error) {
	validLevels := map[int32]bool{1: true, 5: true, 10: true, 60: true, 1440: true}
	if !validLevels[req.FlushIntervalLevel] {
		return &accountingv1.UpdateBufferAccountResponse{Code: 400, Message: "flush_interval_level must be one of: 1, 5, 10, 60, 1440"}, nil
	}
	if err := s.bufferAccountRepo.Update(ctx, req.Id, req.Enabled, model.BufferFlushLevel(req.FlushIntervalLevel), req.Description); err != nil {
		return &accountingv1.UpdateBufferAccountResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.UpdateBufferAccountResponse{Code: 0, Message: "ok"}, nil
}

func (s *Server) DeleteBufferAccount(ctx context.Context, req *accountingv1.DeleteBufferAccountRequest) (*accountingv1.DeleteBufferAccountResponse, error) {
	if err := s.bufferAccountRepo.Delete(ctx, req.Id); err != nil {
		return &accountingv1.DeleteBufferAccountResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.DeleteBufferAccountResponse{Code: 0, Message: "ok"}, nil
}

func toProtoTccBranch(b *service.TccBranchDetail) *accountingv1.TccBranchInfo {
	return &accountingv1.TccBranchInfo{
		TccId:        b.TccID,
		BranchId:     b.BranchID,
		AccountNo:    b.AccountNo,
		BalanceDelta: strconv.FormatInt(b.BalanceDelta, 10),
		FrozenAmount: strconv.FormatInt(b.FrozenAmount, 10),
		Status:       int32(b.Status),
		DbIndex:      int32(b.DBIndex),
		TableIndex:   int32(b.TableIndex),
		CreatedAt:    timestamppb.New(b.CreatedAt),
		UpdatedAt:    timestamppb.New(b.UpdatedAt),
	}
}

// ─── 类型转换 ─────────────────────────────────────────────────────────────────

func toProtoAccount(a *model.Account) *accountingv1.Account {
	balance, _ := currency.FormatAmount(a.Balance, a.Currency)
	frozen, _ := currency.FormatAmount(a.FrozenBalance, a.Currency)
	available, _ := currency.FormatAmount(a.AvailableBalance, a.Currency)
	return &accountingv1.Account{
		AccountNo:           a.AccountNo,
		UserId:              a.UserID,
		AccountType:         convertAccountTypeToProto(a.AccountType),
		Category:            convertAccountCategoryToProto(a.AccountCategory),
		Currency:            a.Currency,
		Balance:             balance,
		FrozenBalance:       frozen,
		AvailableBalance:    available,
		Status:              convertAccountStatusToProto(a.Status),
		Version:             a.Version,
		CreatedAt:           timestamppb.New(a.CreatedAt),
		UpdatedAt:           timestamppb.New(a.UpdatedAt),
		AccountBusinessType: convertAccountBusinessTypeToProto(a.AccountBusinessType),
	}
}

func convertAccountType(t accountingv1.AccountType) model.AccountType {
	m := map[accountingv1.AccountType]model.AccountType{
		accountingv1.AccountType_ACCOUNT_TYPE_USER:                       model.AccountTypeUser,
		accountingv1.AccountType_ACCOUNT_TYPE_MERCHANT:                   model.AccountTypeMerchant,
		accountingv1.AccountType_ACCOUNT_TYPE_MERCHANT_PENDING_SETTLE:    model.AccountTypeMerchantPendingSettle,
		accountingv1.AccountType_ACCOUNT_TYPE_PLATFORM:                   model.AccountTypePlatform,
		accountingv1.AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE: model.AccountTypeTransitChannelReceivable,
		accountingv1.AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE:    model.AccountTypeTransitChannelPayable,
		accountingv1.AccountType_ACCOUNT_TYPE_TRANSACTION_FEE:            model.AccountTypeTransactionFee,
		accountingv1.AccountType_ACCOUNT_TYPE_CHARGE_FEE:                 model.AccountTypeChargeFee,
		accountingv1.AccountType_ACCOUNT_TYPE_TRANSIT:                    model.AccountTypeTransit,
	}
	if v, ok := m[t]; ok {
		return v
	}
	return model.AccountTypeUser
}

func convertAccountTypeToProto(t model.AccountType) accountingv1.AccountType {
	m := map[model.AccountType]accountingv1.AccountType{
		model.AccountTypeUser:                     accountingv1.AccountType_ACCOUNT_TYPE_USER,
		model.AccountTypeMerchant:                 accountingv1.AccountType_ACCOUNT_TYPE_MERCHANT,
		model.AccountTypeMerchantPendingSettle:    accountingv1.AccountType_ACCOUNT_TYPE_MERCHANT_PENDING_SETTLE,
		model.AccountTypePlatform:                 accountingv1.AccountType_ACCOUNT_TYPE_PLATFORM,
		model.AccountTypeTransitChannelReceivable: accountingv1.AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE,
		model.AccountTypeTransitChannelPayable:    accountingv1.AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE,
		model.AccountTypeTransactionFee:           accountingv1.AccountType_ACCOUNT_TYPE_TRANSACTION_FEE,
		model.AccountTypeChargeFee:                accountingv1.AccountType_ACCOUNT_TYPE_CHARGE_FEE,
		model.AccountTypeTransit:                  accountingv1.AccountType_ACCOUNT_TYPE_TRANSIT,
	}
	if v, ok := m[t]; ok {
		return v
	}
	return accountingv1.AccountType_ACCOUNT_TYPE_UNSPECIFIED
}

func convertAccountCategory(c accountingv1.AccountCategory) model.AccountCategory {
	m := map[accountingv1.AccountCategory]model.AccountCategory{
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_ASSET:     model.AccountCategoryAsset,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY: model.AccountCategoryLiability,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_EQUITY:    model.AccountCategoryEquity,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_REVENUE:   model.AccountCategoryRevenue,
		accountingv1.AccountCategory_ACCOUNT_CATEGORY_EXPENSE:   model.AccountCategoryExpense,
	}
	if v, ok := m[c]; ok {
		return v
	}
	return model.AccountCategoryAsset
}

func convertAccountCategoryToProto(c model.AccountCategory) accountingv1.AccountCategory {
	m := map[model.AccountCategory]accountingv1.AccountCategory{
		model.AccountCategoryAsset:     accountingv1.AccountCategory_ACCOUNT_CATEGORY_ASSET,
		model.AccountCategoryLiability: accountingv1.AccountCategory_ACCOUNT_CATEGORY_LIABILITY,
		model.AccountCategoryEquity:    accountingv1.AccountCategory_ACCOUNT_CATEGORY_EQUITY,
		model.AccountCategoryRevenue:   accountingv1.AccountCategory_ACCOUNT_CATEGORY_REVENUE,
		model.AccountCategoryExpense:   accountingv1.AccountCategory_ACCOUNT_CATEGORY_EXPENSE,
	}
	if v, ok := m[c]; ok {
		return v
	}
	return accountingv1.AccountCategory_ACCOUNT_CATEGORY_UNSPECIFIED
}

func convertAccountStatusToProto(s model.AccountStatus) accountingv1.AccountStatus {
	m := map[model.AccountStatus]accountingv1.AccountStatus{
		model.AccountStatusDisabled: accountingv1.AccountStatus_ACCOUNT_STATUS_DISABLED,
		model.AccountStatusActive:   accountingv1.AccountStatus_ACCOUNT_STATUS_ACTIVE,
		model.AccountStatusFrozen:   accountingv1.AccountStatus_ACCOUNT_STATUS_FROZEN,
	}
	if v, ok := m[s]; ok {
		return v
	}
	return accountingv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED
}

func convertAccountBusinessType(t accountingv1.AccountBusinessType) model.AccountBusinessType {
	return model.AccountBusinessType(t)
}

func convertAccountBusinessTypeToProto(t model.AccountBusinessType) accountingv1.AccountBusinessType {
	return accountingv1.AccountBusinessType(t)
}

func convertBusinessType(t accountingv1.BusinessType) model.BusinessType {
	m := map[accountingv1.BusinessType]model.BusinessType{
		accountingv1.BusinessType_BUSINESS_TYPE_TRANSFER:   model.BusinessTypeTransfer,
		accountingv1.BusinessType_BUSINESS_TYPE_PAYMENT:    model.BusinessTypePayment,
		accountingv1.BusinessType_BUSINESS_TYPE_REFUND:     model.BusinessTypeRefund,
		accountingv1.BusinessType_BUSINESS_TYPE_WITHDRAW:   model.BusinessTypeWithdraw,
		accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT:    model.BusinessTypeDeposit,
		accountingv1.BusinessType_BUSINESS_TYPE_COMMISSION: model.BusinessTypeCommission,
	}
	if v, ok := m[t]; ok {
		return v
	}
	return model.BusinessTypeTransfer
}

func convertBusinessTypeToProto(t model.BusinessType) accountingv1.BusinessType {
	m := map[model.BusinessType]accountingv1.BusinessType{
		model.BusinessTypeTransfer:   accountingv1.BusinessType_BUSINESS_TYPE_TRANSFER,
		model.BusinessTypePayment:    accountingv1.BusinessType_BUSINESS_TYPE_PAYMENT,
		model.BusinessTypeRefund:     accountingv1.BusinessType_BUSINESS_TYPE_REFUND,
		model.BusinessTypeWithdraw:   accountingv1.BusinessType_BUSINESS_TYPE_WITHDRAW,
		model.BusinessTypeDeposit:    accountingv1.BusinessType_BUSINESS_TYPE_DEPOSIT,
		model.BusinessTypeCommission: accountingv1.BusinessType_BUSINESS_TYPE_COMMISSION,
	}
	if v, ok := m[t]; ok {
		return v
	}
	return accountingv1.BusinessType_BUSINESS_TYPE_UNSPECIFIED
}

// ─── FreezeService handlers ───────────────────────────────────────────────────

func (s *Server) FreezeBalance(ctx context.Context, req *accountingv1.FreezeBalanceRequest) (*accountingv1.FreezeBalanceResponse, error) {
	if req.OrderNo == "" || req.AccountNo == "" || req.BusinessNo == "" {
		return &accountingv1.FreezeBalanceResponse{Code: 400, Message: "order_no, account_no and business_no are required"}, nil
	}
	if req.Amount <= 0 {
		return &accountingv1.FreezeBalanceResponse{Code: 400, Message: "amount must be positive"}, nil
	}
	if req.Currency == "" {
		req.Currency = "PHP"
	}

	svcReq := &service.FreezeBalanceRequest{
		OrderNo:      req.OrderNo,
		AccountNo:    req.AccountNo,
		BusinessNo:   req.BusinessNo,
		BusinessType: convertBusinessType(req.BusinessType),
		Amount:       req.Amount,
		Currency:     req.Currency,
		Description:  req.Description,
	}
	result, err := s.freezeSvc.FreezeBalance(ctx, svcReq)
	if err != nil {
		s.logger.Error("FreezeBalance failed", zap.Error(err))
		return &accountingv1.FreezeBalanceResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.FreezeBalanceResponse{
		Code:    0,
		Message: "ok",
		OrderNo: result.OrderNo,
	}, nil
}

func (s *Server) UnfreezeAndDebit(ctx context.Context, req *accountingv1.UnfreezeAndDebitRequest) (*accountingv1.UnfreezeAndDebitResponse, error) {
	if req.FreezeOrderNo == "" || req.FreezeAccountNo == "" || req.FreezeBusinessNo == "" {
		return &accountingv1.UnfreezeAndDebitResponse{Code: 400, Message: "freeze_order_no, freeze_account_no and freeze_business_no are required"}, nil
	}
	if len(req.Entries) < 2 {
		return &accountingv1.UnfreezeAndDebitResponse{Code: 400, Message: "at least 2 entries required"}, nil
	}

	entries := make([]service.UnfreezeEntry, len(req.Entries))
	for i, e := range req.Entries {
		entries[i] = service.UnfreezeEntry{
			AccountNo:    e.AccountNo,
			DebitAmount:  e.DebitAmount,
			CreditAmount: e.CreditAmount,
			Description:  e.Description,
		}
	}
	if req.Currency == "" {
		req.Currency = "PHP"
	}

	svcReq := &service.UnfreezeAndDebitRequest{
		FreezeOrderNo:      req.FreezeOrderNo,
		FreezeAccountNo:    req.FreezeAccountNo,
		FreezeBusinessNo:   req.FreezeBusinessNo,
		FreezeBusinessType: convertBusinessType(req.FreezeBusinessType),
		Entries:            entries,
		Currency:           req.Currency,
		Description:        req.Description,
	}
	result, err := s.freezeSvc.UnfreezeAndDebit(ctx, svcReq)
	if err != nil {
		s.logger.Error("UnfreezeAndDebit failed", zap.Error(err))
		return &accountingv1.UnfreezeAndDebitResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.UnfreezeAndDebitResponse{
		Code:           0,
		Message:        "ok",
		VoucherNo:      result.VoucherNo,
		TransactionIds: result.TransactionIDs,
	}, nil
}

func (s *Server) UnfreezeAndReturn(ctx context.Context, req *accountingv1.UnfreezeAndReturnRequest) (*accountingv1.UnfreezeAndReturnResponse, error) {
	if req.FreezeOrderNo == "" || req.FreezeAccountNo == "" || req.FreezeBusinessNo == "" {
		return &accountingv1.UnfreezeAndReturnResponse{Code: 400, Message: "freeze_order_no, freeze_account_no and freeze_business_no are required"}, nil
	}

	svcReq := &service.UnfreezeAndReturnRequest{
		FreezeOrderNo:      req.FreezeOrderNo,
		FreezeAccountNo:    req.FreezeAccountNo,
		FreezeBusinessNo:   req.FreezeBusinessNo,
		FreezeBusinessType: convertBusinessType(req.FreezeBusinessType),
	}
	if err := s.freezeSvc.UnfreezeAndReturn(ctx, svcReq); err != nil {
		s.logger.Error("UnfreezeAndReturn failed", zap.Error(err))
		return &accountingv1.UnfreezeAndReturnResponse{Code: 500, Message: err.Error()}, nil
	}
	return &accountingv1.UnfreezeAndReturnResponse{Code: 0, Message: "ok"}, nil
}

// resolveEntryAmounts 从 AccountingEntry 解出借贷存储值。优先走新 Money 字段
// （minor_units → storage），否则回落旧 debit_amount / credit_amount（decimal
// string → storage）。两种都能同时出现时 Money 胜出。
func resolveEntryAmounts(e *accountingv1.AccountingEntry, currencyCode string) (debit int64, credit int64, err error) {
	if e == nil {
		return 0, 0, fmt.Errorf("nil entry")
	}
	if m := e.GetDebitMoney(); m != nil {
		cur := m.GetCurrency()
		if cur == "" {
			cur = currencyCode
		}
		debit, err = currency.ToStorageFromMinor(m.GetMinorUnits(), cur)
		if err != nil {
			return 0, 0, fmt.Errorf("debit_money: %w", err)
		}
	} else {
		dec, derr := decimal.NewFromString(e.GetDebitAmount())
		if derr != nil {
			return 0, 0, fmt.Errorf("invalid debit_amount %q: %w", e.GetDebitAmount(), derr)
		}
		debit, err = currency.ToStorage(dec, currencyCode)
		if err != nil {
			return 0, 0, fmt.Errorf("debit_amount: %w", err)
		}
	}
	if m := e.GetCreditMoney(); m != nil {
		cur := m.GetCurrency()
		if cur == "" {
			cur = currencyCode
		}
		credit, err = currency.ToStorageFromMinor(m.GetMinorUnits(), cur)
		if err != nil {
			return 0, 0, fmt.Errorf("credit_money: %w", err)
		}
	} else {
		dec, derr := decimal.NewFromString(e.GetCreditAmount())
		if derr != nil {
			return 0, 0, fmt.Errorf("invalid credit_amount %q: %w", e.GetCreditAmount(), derr)
		}
		credit, err = currency.ToStorage(dec, currencyCode)
		if err != nil {
			return 0, 0, fmt.Errorf("credit_amount: %w", err)
		}
	}
	return debit, credit, nil
}

// ─── RESTORE-7 / TECH-DEBT-1/3/4: admin-web + split-payment 用 RPC ──────────
//
// 历史精简后这批 RPC 一度只剩 stub 骨架; 现已全部接到真 service/repo:
//   - TECH-DEBT-1: ListAccountsByUserAndBusinessType / ListDayCutHistory /
//     ListSnapshotDates / ListAccountTypes / ListTransactionRules.
//   - TECH-DEBT-3: CreateTransaction multi-leg (Legs[] 已加进 wire proto,
//     server 端展开 借/贷 entries 走 DoubleEntryBooking).
//   - TECH-DEBT-4: RebuildHotAccounts 透传到 accountingSvc.RebuildHotAccounts
//     (跨分片重算 + 写 Redis).

// ListAccountsByUserAndBusinessType 按 (userID, businessType, currency) 列账户.
// currency 空表示返回该 (user, businessType) 下所有币种. 上层 admin-web 用此 RPC
// 在 user_topup 场景查"这个 user 当前已经开了哪些 business_type 账户" (designer
// picker / 账户列表页).
func (s *Server) ListAccountsByUserAndBusinessType(ctx context.Context, req *accountingv1.ListAccountsByUserAndBusinessTypeRequest) (*accountingv1.ListAccountsByUserAndBusinessTypeResponse, error) {
	if req == nil || req.GetUserId() <= 0 {
		return &accountingv1.ListAccountsByUserAndBusinessTypeResponse{
			Code:    1,
			Message: "user_id required",
		}, nil
	}
	accs, err := s.accountingSvc.ListAccountsByUserAndBusinessType(ctx,
		req.GetUserId(),
		convertAccountBusinessType(req.GetAccountBusinessType()),
		req.GetCurrency(),
	)
	if err != nil {
		s.logger.Warn("ListAccountsByUserAndBusinessType failed",
			zap.Int64("user_id", req.GetUserId()),
			zap.String("currency", req.GetCurrency()),
			zap.Error(err))
		return &accountingv1.ListAccountsByUserAndBusinessTypeResponse{
			Code:    1,
			Message: err.Error(),
		}, nil
	}
	out := make([]*accountingv1.Account, 0, len(accs))
	for _, a := range accs {
		out = append(out, toProtoAccount(a))
	}
	return &accountingv1.ListAccountsByUserAndBusinessTypeResponse{
		Code:     0,
		Message:  "ok",
		Accounts: out,
	}, nil
}

// ListDayCutHistory 跨 100 个分片聚合 day_cut_control, 按 (cut_date, run_id,
// currency) 折叠为一行. 上层 admin-web "日切历史"页面用. from_date/to_date 在
// service 层之外补充过滤 (字符串日期 "YYYY-MM-DD" 字典序即时间序). limit<=0
// 表示不限.
func (s *Server) ListDayCutHistory(ctx context.Context, req *accountingv1.ListDayCutHistoryRequest) (*accountingv1.ListDayCutHistoryResponse, error) {
	entries, err := s.dayCutSvc.ListDayCutHistory(ctx)
	if err != nil {
		s.logger.Warn("ListDayCutHistory failed", zap.Error(err))
		return &accountingv1.ListDayCutHistoryResponse{Code: 1, Message: err.Error()}, nil
	}
	fromDate := req.GetFromDate()
	toDate := req.GetToDate()
	limit := int(req.GetLimit())
	out := make([]*accountingv1.DayCutHistoryEntry, 0, len(entries))
	for _, e := range entries {
		if fromDate != "" && e.CutDate < fromDate {
			continue
		}
		if toDate != "" && e.CutDate > toDate {
			continue
		}
		out = append(out, &accountingv1.DayCutHistoryEntry{
			CutDate:     e.CutDate,
			RunId:       int32(e.RunID),
			Currency:    e.Currency,
			TotalShards: int32(e.TotalShards),
			Pending:     int32(e.Pending),
			Processing:  int32(e.Processing),
			Completed:   int32(e.Completed),
			Failed:      int32(e.Failed),
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return &accountingv1.ListDayCutHistoryResponse{
		Code:    0,
		Message: "ok",
		Entries: out,
	}, nil
}

// ListSnapshotDates 列出已有 balance snapshot 的所有日期 (DESC), 用于 admin-web
// "试算平衡"页面的日期下拉. trialBalanceSvc 已封好跨分片 distinct 聚合.
func (s *Server) ListSnapshotDates(ctx context.Context, _ *accountingv1.ListSnapshotDatesRequest) (*accountingv1.ListSnapshotDatesResponse, error) {
	dates, err := s.trialBalanceSvc.ListSnapshotDates(ctx)
	if err != nil {
		s.logger.Warn("ListSnapshotDates failed", zap.Error(err))
		return &accountingv1.ListSnapshotDatesResponse{Code: 1, Message: err.Error()}, nil
	}
	return &accountingv1.ListSnapshotDatesResponse{
		Code:    0,
		Message: "ok",
		Dates:   dates,
	}, nil
}

// ListAccountTypes 返回 account_type_info 全表 (条目少, 不走缓存). skeleton proto
// 只暴露 code/name/description, 不带 is_platform/owner_type/balance_direction;
// 真要这些字段就走 adminhttp /admin/account-types (admin-web 的 BFF 用).
func (s *Server) ListAccountTypes(ctx context.Context, _ *accountingv1.ListAccountTypesRequest) (*accountingv1.ListAccountTypesResponse, error) {
	rows, err := s.ruleRepo.ListAccountTypes(ctx)
	if err != nil {
		s.logger.Warn("ListAccountTypes failed", zap.Error(err))
		return &accountingv1.ListAccountTypesResponse{}, err
	}
	items := make([]*accountingv1.AccountTypeItem, 0, len(rows))
	for _, r := range rows {
		desc := r.Description
		if desc == "" {
			desc = r.AccountTypeDesc
		}
		items = append(items, &accountingv1.AccountTypeItem{
			Code:        r.AccountType,
			Name:        r.AccountTypeName,
			Description: desc,
		})
	}
	return &accountingv1.ListAccountTypesResponse{Items: items}, nil
}

// ListTransactionRules 按 product_code 拉规则; 空 = 全部. split-payment 在每次
// SaveGraph + Engine 加载时缓存 5min, 这条 RPC 是热路径.
func (s *Server) ListTransactionRules(ctx context.Context, req *accountingv1.ListTransactionRulesRequest) (*accountingv1.ListTransactionRulesResponse, error) {
	rules, err := s.ruleRepo.ListRulesByProduct(ctx, req.GetProductFilter())
	if err != nil {
		s.logger.Warn("ListTransactionRules failed",
			zap.String("product", req.GetProductFilter()),
			zap.Error(err))
		return &accountingv1.ListTransactionRulesResponse{}, err
	}
	items := make([]*accountingv1.TransactionRuleItem, 0, len(rules))
	for _, r := range rules {
		items = append(items, &accountingv1.TransactionRuleItem{
			Id:              r.ID,
			ProductCode:     r.ProductCode,
			EventCode:       r.EventCode,
			DebitSubjectId:  r.DebitSubjectID,
			CreditSubjectId: r.CreditSubjectID,
			FromDirection:   r.FromDirection,
			ToDirection:     r.ToDirection,
			// model.TransactionRule 没 Description 列, 留空 (Extra JSON 里有 desc 时另说).
		})
	}
	return &accountingv1.ListTransactionRulesResponse{Items: items}, nil
}

// ─── TECH-DEBT-3 / TECH-DEBT-4 已实装 (见各自 handler 注释) ───────────────────

// RebuildHotAccounts (TECH-DEBT-4):
//
// 透传到 accountingSvc.RebuildHotAccounts (跨分片重算 + 写 Redis). AsOf 支持
// 三种格式: 空 = now, "5m"/"2h" 相对时长, "YYYY-MM-DD" 或 RFC3339 绝对时间.
// 与 adminhttp /admin/redis/rebuild 是同一条 service 入口, 两路一致.
func (s *Server) RebuildHotAccounts(ctx context.Context, req *accountingv1.RebuildHotAccountsRequest) (*accountingv1.RebuildHotAccountsResponse, error) {
	opts := service.RebuildOptions{
		AccountNos: req.GetAccountNos(),
		DryRun:     req.GetDryRun(),
	}
	if asOf := strings.TrimSpace(req.GetAsOf()); asOf != "" {
		switch {
		case parseAsRelative(asOf, &opts.AsOf):
			// duration
		case parseAsDate(asOf, &opts.AsOf):
			// YYYY-MM-DD
		case parseAsRFC3339(asOf, &opts.AsOf):
			// RFC3339
		default:
			return &accountingv1.RebuildHotAccountsResponse{
				Code:    400,
				Message: fmt.Sprintf("as_of 必须是 duration / YYYY-MM-DD / RFC3339; got %q", asOf),
				AsOf:    asOf,
				DryRun:  opts.DryRun,
			}, nil
		}
	}
	report, err := s.accountingSvc.RebuildHotAccounts(ctx, opts)
	if err != nil {
		s.logger.Warn("RebuildHotAccounts failed", zap.Error(err))
		return &accountingv1.RebuildHotAccountsResponse{
			Code:    500,
			Message: err.Error(),
			AsOf:    req.GetAsOf(),
			DryRun:  req.GetDryRun(),
		}, nil
	}
	entries := make([]*accountingv1.RebuildHotAccountEntry, 0, len(report.Entries))
	for _, e := range report.Entries {
		entries = append(entries, &accountingv1.RebuildHotAccountEntry{
			AccountNo:     e.AccountNo,
			BalanceBefore: e.BalanceBefore,
			BalanceAfter:  e.BalanceAfter,
			Source:        e.Source,
			JournalCutoff: e.JournalCutoff,
			Skipped:       e.Skipped,
			Reason:        e.Reason,
		})
	}
	return &accountingv1.RebuildHotAccountsResponse{
		Code:     0,
		Message:  "ok",
		AsOf:     report.AsOf.Format(time.RFC3339),
		DryRun:   report.DryRun,
		Total:    int32(report.Total),
		Updated:  int32(report.Updated),
		Skipped:  int32(report.Skipped),
		Failed:   int32(report.Failed),
		Duration: report.Duration,
		Entries:  entries,
	}, nil
}

func parseAsRelative(s string, out *time.Time) bool {
	d, err := time.ParseDuration(s)
	if err != nil {
		return false
	}
	*out = time.Now().Add(-d)
	return true
}

func parseAsDate(s string, out *time.Time) bool {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return false
	}
	*out = t
	return true
}

func parseAsRFC3339(s string, out *time.Time) bool {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return false
	}
	*out = t
	return true
}

// CreateTransaction (TECH-DEBT-3 v1):
//
// 接收 split-payment translator 派生好的 Legs[], 直接落 multi-leg double-entry.
// 每个 leg 展开为 2 行 AccountingEntry (from = 借方, to = 贷方), 全部 leg 必须
// 同币种 (currency 取首 leg, 与剩余 leg 校验); idempotency_key 给 accounting
// 侧幂等. legs 空 → 返 400 让 caller 升级 client.
//
// 与 Legs 路径正交的"按 (product_code, event_code) 查 rule 派生 entries" 的真
// rule-engine, 留给后续 PR; 当前 caller (split-payment translator + order-core
// mapper) 都已经在自己这一侧解析了 account_no, 上来就喂 Legs 即可.
func (s *Server) CreateTransaction(ctx context.Context, req *accountingv1.CreateTransactionRequest) (*accountingv1.CreateTransactionResponse, error) {
	if req == nil {
		return &accountingv1.CreateTransactionResponse{
			Status:       "failed",
			ErrorMessage: "nil request",
		}, nil
	}
	if req.GetIdempotencyKey() == "" {
		return &accountingv1.CreateTransactionResponse{
			OrderNo:      req.GetBusinessNo(),
			Status:       "failed",
			ErrorMessage: "idempotency_key required",
		}, nil
	}
	legs := req.GetLegs()
	if len(legs) == 0 {
		// Rule-engine derivation 兜底: 当 caller 没派生 legs 时, 让 server 按
		// (product_code, event_code) 查 transaction_rule 当文档/审计回执用. 真要
		// 据 rule 自动派生 account_no + amount 需要 caller 再传 (from_party_id,
		// to_party_id, amount, currency) — proto 当前没有这些字段, 所以这里只
		// 把命中的 rule 行回 error message, 让 caller 能据此 debug + 自行扩展.
		var hint string
		if req.GetProductCode() != "" {
			if rules, err := s.ruleRepo.ListRulesByProduct(ctx, req.GetProductCode()); err == nil {
				matched := 0
				for _, r := range rules {
					if r.EventCode == "" || r.EventCode == req.GetEventCode() {
						matched++
					}
				}
				hint = fmt.Sprintf(" (matched %d rule(s) for product=%s event=%s; caller must derive legs from these)", matched, req.GetProductCode(), req.GetEventCode())
			}
		}
		return &accountingv1.CreateTransactionResponse{
			OrderNo:      req.GetIdempotencyKey(),
			Status:       "failed",
			ErrorMessage: "legs empty" + hint,
		}, nil
	}

	// 校验同币种 + 累计 entries
	currency := strings.TrimSpace(legs[0].GetCurrency())
	if currency == "" {
		return &accountingv1.CreateTransactionResponse{
			OrderNo:      req.GetIdempotencyKey(),
			Status:       "failed",
			ErrorMessage: "leg[0].currency required",
		}, nil
	}
	// RUNTIME-FIX-1: 按 account_no 聚合 debit/credit 净额, 同账户多次出现只生成
	// 一条 entry (DoubleEntryBooking.validateEntries 强制每笔 booking 内 account_no
	// 唯一). 中转户进出相抵 net=0 直接跳过. 跟 transactionService.executeBookkeeping
	// 同款逻辑, 之前 Kitex handler 没复用导致 raw 2-per-leg entry 提交被 reject.
	type accSummary struct {
		debit, credit int64
		description   string
	}
	agg := map[string]*accSummary{}
	defaultDesc := req.GetRemark()
	for i, leg := range legs {
		legCur := strings.TrimSpace(leg.GetCurrency())
		if legCur == "" {
			legCur = currency
		}
		if legCur != currency {
			return &accountingv1.CreateTransactionResponse{
				OrderNo:      req.GetIdempotencyKey(),
				Status:       "failed",
				ErrorMessage: fmt.Sprintf("leg[%d] currency=%s mismatch first leg=%s", i, legCur, currency),
			}, nil
		}
		if leg.GetAmountMinor() <= 0 {
			return &accountingv1.CreateTransactionResponse{
				OrderNo:      req.GetIdempotencyKey(),
				Status:       "failed",
				ErrorMessage: fmt.Sprintf("leg[%d] amount_minor must be > 0", i),
			}, nil
		}
		if leg.GetFromAccountNo() == "" || leg.GetToAccountNo() == "" {
			return &accountingv1.CreateTransactionResponse{
				OrderNo:      req.GetIdempotencyKey(),
				Status:       "failed",
				ErrorMessage: fmt.Sprintf("leg[%d] from_account_no / to_account_no required", i),
			}, nil
		}
		if leg.GetFromAccountNo() == leg.GetToAccountNo() {
			return &accountingv1.CreateTransactionResponse{
				OrderNo:      req.GetIdempotencyKey(),
				Status:       "failed",
				ErrorMessage: fmt.Sprintf("leg[%d] from == to (%s); self-transfer not allowed", i, leg.GetFromAccountNo()),
			}, nil
		}
		desc := leg.GetDescription()
		if desc == "" {
			desc = defaultDesc
		}
		amount := leg.GetAmountMinor()
		fromAcc := leg.GetFromAccountNo()
		toAcc := leg.GetToAccountNo()
		if agg[fromAcc] == nil {
			agg[fromAcc] = &accSummary{description: desc}
		}
		agg[fromAcc].debit += amount
		if agg[toAcc] == nil {
			agg[toAcc] = &accSummary{description: desc}
		}
		agg[toAcc].credit += amount
	}
	entries := make([]service.AccountingEntry, 0, len(agg))
	for accNo, sum := range agg {
		net := sum.debit - sum.credit
		switch {
		case net > 0:
			entries = append(entries, service.AccountingEntry{
				AccountNo:    accNo,
				DebitAmount:  net,
				CreditAmount: 0,
				Description:  sum.description,
			})
		case net < 0:
			entries = append(entries, service.AccountingEntry{
				AccountNo:    accNo,
				DebitAmount:  0,
				CreditAmount: -net,
				Description:  sum.description,
			})
			// net == 0: 中转户进出相抵, 不影响余额, 跳过 entry.
		}
	}
	if len(entries) == 0 {
		return &accountingv1.CreateTransactionResponse{
			OrderNo:      req.GetIdempotencyKey(),
			Status:       "failed",
			ErrorMessage: "all legs aggregated to net 0 (no real money movement)",
		}, nil
	}

	businessNo := req.GetBusinessNo()
	if businessNo == "" {
		businessNo = req.GetIdempotencyKey()
	}
	remark := req.GetRemark()
	if remark == "" {
		remark = fmt.Sprintf("%s/%s", req.GetProductCode(), req.GetEventCode())
	}
	voucherNo, _, err := s.accountingSvc.DoubleEntryBooking(ctx, &service.DoubleEntryBookingRequest{
		RequestID:    req.GetIdempotencyKey(),
		BusinessNo:   businessNo,
		BusinessType: eventCodeToBusinessType(req.GetEventCode()),
		Entries:      entries,
		Currency:     currency,
		Description:  remark,
	})
	if err != nil {
		if errors.Is(err, service.ErrRequestInProgress) {
			return &accountingv1.CreateTransactionResponse{
				OrderNo:      req.GetIdempotencyKey(),
				Status:       "pending",
				ErrorMessage: err.Error(),
			}, nil
		}
		s.logger.Warn("CreateTransaction failed",
			zap.String("business_no", businessNo),
			zap.String("product", req.GetProductCode()),
			zap.String("event", req.GetEventCode()),
			zap.Int("legs", len(legs)),
			zap.Error(err))
		return &accountingv1.CreateTransactionResponse{
			OrderNo:      req.GetIdempotencyKey(),
			Status:       "failed",
			ErrorMessage: err.Error(),
		}, nil
	}
	return &accountingv1.CreateTransactionResponse{
		OrderNo:   req.GetIdempotencyKey(),
		Status:    "posted",
		VoucherNo: voucherNo,
	}, nil
}

// eventCodeToBusinessType 把 split-payment event_code 字符串 (e.g. "charge.
// succeeded", "refund.succeeded", "transfer.posted") 映射到 accounting 内部
// BusinessType 枚举. 未识别归 TRANSFER, 保守不阻断 (transaction_order 主键不靠它).
func eventCodeToBusinessType(event string) model.BusinessType {
	switch {
	case strings.HasPrefix(event, "charge."), strings.HasPrefix(event, "payment."), strings.HasPrefix(event, "topup."):
		return model.BusinessTypePayment
	case strings.HasPrefix(event, "refund."), strings.HasPrefix(event, "reversal."):
		return model.BusinessTypeRefund
	case strings.HasPrefix(event, "withdraw."), strings.HasPrefix(event, "payout."):
		return model.BusinessTypeWithdraw
	case strings.HasPrefix(event, "deposit."):
		return model.BusinessTypeDeposit
	case strings.HasPrefix(event, "commission."), strings.HasPrefix(event, "fee."):
		return model.BusinessTypeCommission
	default:
		return model.BusinessTypeTransfer
	}
}
