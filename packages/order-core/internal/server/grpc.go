// Package server Kitex 适配层 (Stripe-API 风格, multi-service).
package server

import (
	"context"
	"errors"
	"fmt"
	"net"

	kitexserver "github.com/cloudwego/kitex/server"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	orderv1 "github.com/xiongwp/order-core/kitex_gen/order/v1"
	auditservice "github.com/xiongwp/order-core/kitex_gen/order/v1/auditservice"
	chargeservice "github.com/xiongwp/order-core/kitex_gen/order/v1/chargeservice"
	disputeservice "github.com/xiongwp/order-core/kitex_gen/order/v1/disputeservice"
	ledgerservice "github.com/xiongwp/order-core/kitex_gen/order/v1/ledgerservice"
	paymentintentservice "github.com/xiongwp/order-core/kitex_gen/order/v1/paymentintentservice"
	refundservice "github.com/xiongwp/order-core/kitex_gen/order/v1/refundservice"
	webhookdeliveryservice "github.com/xiongwp/order-core/kitex_gen/order/v1/webhookdeliveryservice"
	webhookservice "github.com/xiongwp/order-core/kitex_gen/order/v1/webhookservice"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/repo"
	"github.com/xiongwp/order-core/internal/service"
	"github.com/xiongwp/order-core/internal/webhook"
)

// Server 实现 PaymentIntentService. Charge / Refund / Webhook 走独立 forwarder 以避免方法名冲突.
// 切 Kitex 后不再 embed UnimplementedPaymentIntentServiceServer.
type Server struct {
	piSvc       service.PaymentIntentService
	chargeSvc   service.ChargeService
	refundSvc   service.RefundService
	actionSvc   service.PayActionService
	webhookSvc  service.WebhookService
	ledgerSvc   service.LedgerService
	disputeSvc  service.DisputeService
	auditRepo            repo.AdminAuditRepository
	dbMgr                *repo.Manager
	webhookDisp          *webhook.Dispatcher
	authTokens           map[string]string
	authAllowUnauthenticated bool
	rateLimit            float64
	rateBurst            int
	logger               *zap.Logger

	// kitexSrv ListenAndServe 期间持有; Stop() 用来 graceful Stop.
	kitexSrv kitexserver.Server
	done     chan struct{}
}

// Stop 优雅关停 Kitex server. SIGTERM 时由 fx OnStop 调用.
// Kitex Stop() 内部已 graceful (等 in-flight RPC); 用 select 限上限.
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

// Deps gRPC server 的依赖
type Deps struct {
	PISvc      service.PaymentIntentService
	ChargeSvc  service.ChargeService
	RefundSvc  service.RefundService
	ActionSvc  service.PayActionService
	WebhookSvc service.WebhookService
	LedgerSvc  service.LedgerService
	DisputeSvc service.DisputeService
	AuditRepo  repo.AdminAuditRepository
	DBMgr      *repo.Manager
	WebhookDisp *webhook.Dispatcher
	AuthTokens           map[string]string
	AuthAllowUnauthenticated bool
	RateLimitRPS         float64
	RateBurst            int
	Logger               *zap.Logger
}

// NewServer 构造。
//
// 安全约束：若 d.AuthTokens 为空且 d.AuthAllowUnauthenticated=false，返回 error 阻止启动，
// 避免配置漏注入导致 gRPC 全 RPC 裸奔。dev/单测场景需显式传 AuthAllowUnauthenticated=true。
func NewServer(d Deps) (*Server, error) {
	if d.Logger == nil {
		d.Logger = zap.NewNop()
	}
	if len(d.AuthTokens) == 0 && !d.AuthAllowUnauthenticated {
		return nil, errors.New("server: AuthTokens is empty and AuthAllowUnauthenticated=false; refusing to start. Set auth.tokens or explicitly set auth.allow_unauthenticated=true for dev only")
	}
	return &Server{
		piSvc:                    d.PISvc,
		chargeSvc:                d.ChargeSvc,
		refundSvc:                d.RefundSvc,
		actionSvc:                d.ActionSvc,
		webhookSvc:               d.WebhookSvc,
		ledgerSvc:                d.LedgerSvc,
		disputeSvc:               d.DisputeSvc,
		auditRepo:                d.AuditRepo,
		dbMgr:                    d.DBMgr,
		webhookDisp:              d.WebhookDisp,
		authTokens:               d.AuthTokens,
		authAllowUnauthenticated: d.AuthAllowUnauthenticated,
		rateLimit:                d.RateLimitRPS,
		rateBurst:                d.RateBurst,
		logger:                   d.Logger,
	}, nil
}

// ListenAndServe 启动 Kitex multi-service server (阻塞).
//
// 注册 8 个 service 到同一个端口 (Kitex 0.10+ MultiService):
//   PaymentIntent / Charge / Refund / Webhook (主链路)
//   Audit / WebhookDelivery / Ledger / Dispute (可选, 依赖配置)
//
// TODO: kitexutil MW (Recover / Trace / Shadow / Logging / Metrics / RateLimit / Auth)
// — 等 kitexutil port 完成后接 server.WithMiddleware(...).
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("resolve addr :%d: %w", port, err)
	}
	gs := kitexserver.NewServer(kitexserver.WithServiceAddr(addr))

	// 主链路 4 个 service — 一定注册
	paymentintentservice.RegisterService(gs, s)
	chargeservice.RegisterService(gs, NewChargeForwarder(s))
	refundservice.RegisterService(gs, NewRefundForwarder(s))
	webhookservice.RegisterService(gs, NewWebhookForwarder(s))

	// 可选 service — 依赖配置注入
	if s.auditRepo != nil {
		auditservice.RegisterService(gs, NewAuditServer(s.auditRepo))
	}
	if s.dbMgr != nil && s.webhookDisp != nil {
		webhookdeliveryservice.RegisterService(gs,
			NewWebhookDeliveryServer(s.dbMgr, s.webhookDisp))
	}
	if s.ledgerSvc != nil {
		ledgerservice.RegisterService(gs, NewLedgerServer(s.ledgerSvc))
	}
	if s.disputeSvc != nil {
		disputeservice.RegisterService(gs, NewDisputeServer(s.disputeSvc))
	}

	// 健康检查 / reflection 由 Kitex 框架自带, 不再手动注册.
	_ = health.NewServer
	_ = grpc_health_v1.HealthCheckResponse_SERVING
	_ = reflection.Register

	s.kitexSrv = gs
	if s.done == nil {
		s.done = make(chan struct{})
	}
	go func() { <-ctx.Done(); _ = gs.Stop() }()
	s.logger.Info("order-core Kitex listening", zap.String("addr", addr.String()))
	err = gs.Run()
	close(s.done)
	return err
}

// ─── PaymentIntentService ─────────────────────────────────────────────────────

func (s *Server) Create(ctx context.Context, req *orderv1.CreatePaymentIntentRequest) (*orderv1.CreatePaymentIntentResponse, error) {
	if req.GetAmount() <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	if req.GetCurrency() == "" {
		return nil, fmt.Errorf("currency required")
	}
	if req.GetMchId() == "" {
		return nil, fmt.Errorf("mch_id required")
	}
	pi, err := s.piSvc.Create(ctx, &service.CreatePaymentIntentInput{
		Amount:              req.GetAmount(),
		Currency:            req.GetCurrency(),
		CustomerID:          req.GetCustomerId(),
		Description:         req.GetDescription(),
		MchID:               req.GetMchId(),
		MchOrderNo:          req.GetMchOrderNo(),
		BusinessID:          req.GetBusinessId(),
		IdempotencyKey:      req.GetIdempotencyKey(),
		CaptureMethod:       captureMethodFromProto(req.GetCaptureMethod()),
		ConfirmationMethod:  confirmationMethodFromProto(req.GetConfirmationMethod()),
		PaymentMethodTypes:  req.GetPaymentMethodTypes(),
		ReturnURL:           req.GetReturnUrl(),
		NotifyURL:           req.GetNotifyUrl(),
		StatementDescriptor: req.GetStatementDescriptor(),
		Metadata:            req.GetMetadata(),
		Livemode:            req.GetLivemode(),
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.CreatePaymentIntentResponse{PaymentIntent: piToProto(pi)}, nil
}

func (s *Server) Retrieve(ctx context.Context, req *orderv1.RetrievePaymentIntentRequest) (*orderv1.RetrievePaymentIntentResponse, error) {
	if req.GetId() == "" {
		return nil, fmt.Errorf("id required")
	}
	pi, err := s.piSvc.Retrieve(ctx, req.GetId())
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.RetrievePaymentIntentResponse{PaymentIntent: piToProto(pi)}, nil
}

func (s *Server) Update(ctx context.Context, req *orderv1.UpdatePaymentIntentRequest) (*orderv1.UpdatePaymentIntentResponse, error) {
	pi, err := s.piSvc.Update(ctx, req.GetId(), req.GetDescription(), req.GetMetadata())
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.UpdatePaymentIntentResponse{PaymentIntent: piToProto(pi)}, nil
}

func (s *Server) Confirm(ctx context.Context, req *orderv1.ConfirmPaymentIntentRequest) (*orderv1.ConfirmPaymentIntentResponse, error) {
	if req.GetId() == "" {
		return nil, fmt.Errorf("id required")
	}
	if splits := req.GetPaymentMethods(); len(splits) > 0 {
		in := make([]service.PaymentSplit, 0, len(splits))
		for _, sp := range splits {
			in = append(in, service.PaymentSplit{
				PaymentMethod:    sp.GetPaymentMethod(),
				Amount:           sp.GetAmount(),
				PaymentMethodRef: sp.GetPaymentMethodRef(),
				Extra:            sp.GetExtra(),
			})
		}
		pi, charges, err := s.piSvc.ConfirmCombined(ctx, req.GetId(), req.GetClientSecret(), in)
		if err != nil {
			return nil, mapError(err)
		}
		nextAct, _ := s.actionSvc.PendingByPI(ctx, pi.ID)
		out := &orderv1.ConfirmPaymentIntentResponse{
			PaymentIntent: piToProto(pi),
			Charges:       chargesToProto(charges),
			NextAction:    payActionToProto(nextAct),
		}
		if len(charges) > 0 {
			out.Charge = chargeToProto(charges[0])
		}
		return out, nil
	}
	pi, ch, err := s.piSvc.Confirm(ctx, req.GetId(), req.GetPaymentMethod(), req.GetClientSecret())
	if err != nil {
		return nil, mapError(err)
	}
	nextAct, _ := s.actionSvc.PendingByPI(ctx, pi.ID)
	return &orderv1.ConfirmPaymentIntentResponse{
		PaymentIntent: piToProto(pi),
		Charge:        chargeToProto(ch),
		Charges:       []*orderv1.Charge{chargeToProto(ch)},
		NextAction:    payActionToProto(nextAct),
	}, nil
}

func (s *Server) ConfirmPaymentAction(ctx context.Context, req *orderv1.ConfirmPaymentActionRequest) (*orderv1.ConfirmPaymentActionResponse, error) {
	action, pi, err := s.actionSvc.Submit(ctx, req.GetId(), req.GetActionId(), req.GetVerificationData())
	if err != nil {
		return nil, mapError(err)
	}
	resp := &orderv1.ConfirmPaymentActionResponse{
		PaymentIntent: piToProto(pi),
		Action:        payActionToProto(action),
	}
	if pi != nil {
		if next, err := s.actionSvc.PendingByPI(ctx, pi.ID); err == nil && next != nil && next.ID != action.ID {
			resp.NextAction = payActionToProto(next)
		}
		if pi.Status == domain.PIStatusSucceeded && len(pi.ActiveChargeIDs) == 0 {
			if list, err := s.chargeSvc.ListByPI(ctx, pi.ID); err == nil && len(list) > 0 {
				resp.Charge = chargeToProto(list[len(list)-1])
			}
		}
	}
	return resp, nil
}

func (s *Server) Capture(ctx context.Context, req *orderv1.CapturePaymentIntentRequest) (*orderv1.CapturePaymentIntentResponse, error) {
	pi, ch, err := s.piSvc.Capture(ctx, req.GetId(), req.GetAmountToCapture())
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.CapturePaymentIntentResponse{PaymentIntent: piToProto(pi), Charge: chargeToProto(ch)}, nil
}

func (s *Server) Cancel(ctx context.Context, req *orderv1.CancelPaymentIntentRequest) (*orderv1.CancelPaymentIntentResponse, error) {
	pi, err := s.piSvc.Cancel(ctx, req.GetId(), req.GetCancellationReason())
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.CancelPaymentIntentResponse{PaymentIntent: piToProto(pi)}, nil
}

func (s *Server) List(ctx context.Context, req *orderv1.ListPaymentIntentsRequest) (*orderv1.ListPaymentIntentsResponse, error) {
	list, total, err := s.piSvc.List(ctx, req.GetMchId(), int(req.GetPage()), int(req.GetPageSize()))
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*orderv1.PaymentIntent, 0, len(list))
	for _, pi := range list {
		out = append(out, piToProto(pi))
	}
	return &orderv1.ListPaymentIntentsResponse{PaymentIntents: out, Total: total}, nil
}

// ─── forwarder: ChargeService ────────────────────────────────────────────────

type ChargeForwarder struct {
	s *Server
}

func NewChargeForwarder(s *Server) *ChargeForwarder { return &ChargeForwarder{s: s} }

func (c *ChargeForwarder) Retrieve(ctx context.Context, req *orderv1.RetrieveChargeRequest) (*orderv1.RetrieveChargeResponse, error) {
	ch, err := c.s.chargeSvc.RetrieveByChargeID(ctx, req.GetId())
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.RetrieveChargeResponse{Charge: chargeToProto(ch)}, nil
}

func (c *ChargeForwarder) List(ctx context.Context, req *orderv1.ListChargesRequest) (*orderv1.ListChargesResponse, error) {
	list, err := c.s.chargeSvc.ListByPI(ctx, req.GetPaymentIntentId())
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.ListChargesResponse{Charges: chargesToProto(list)}, nil
}

// ─── forwarder: RefundService ────────────────────────────────────────────────

type RefundForwarder struct {
	s *Server
}

func NewRefundForwarder(s *Server) *RefundForwarder { return &RefundForwarder{s: s} }

func (f *RefundForwarder) Create(ctx context.Context, req *orderv1.CreateRefundRequest) (*orderv1.CreateRefundResponse, error) {
	in := &service.CreateRefundInput{
		PaymentIntentID: req.GetPaymentIntentId(),
		ChargeID:        req.GetChargeId(),
		Amount:          req.GetAmount(),
		Reason:          refundReasonFromProto(req.GetReason()),
		Metadata:        req.GetMetadata(),
	}
	rf, err := f.s.refundSvc.Create(ctx, in)
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.CreateRefundResponse{Refund: refundToProto(rf)}, nil
}

func (f *RefundForwarder) Retrieve(ctx context.Context, req *orderv1.RetrieveRefundRequest) (*orderv1.RetrieveRefundResponse, error) {
	rf, err := f.s.refundSvc.RetrieveByRefundID(ctx, req.GetId())
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.RetrieveRefundResponse{Refund: refundToProto(rf)}, nil
}

func (f *RefundForwarder) List(ctx context.Context, req *orderv1.ListRefundsRequest) (*orderv1.ListRefundsResponse, error) {
	var list []*domain.Refund
	var err error
	if req.GetPaymentIntentId() != "" {
		list, err = f.s.refundSvc.ListByPI(ctx, req.GetPaymentIntentId())
	}
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*orderv1.Refund, 0, len(list))
	for _, r := range list {
		out = append(out, refundToProto(r))
	}
	return &orderv1.ListRefundsResponse{Refunds: out}, nil
}

// ─── forwarder: WebhookService ───────────────────────────────────────────────

type WebhookForwarder struct {
	s *Server
}

func NewWebhookForwarder(s *Server) *WebhookForwarder { return &WebhookForwarder{s: s} }

func (w *WebhookForwarder) Ingest(ctx context.Context, req *orderv1.IngestWebhookRequest) (*orderv1.IngestWebhookResponse, error) {
	ev, err := w.s.webhookSvc.Ingest(ctx, &service.IngestWebhookInput{
		Direction:   service.WebhookDirection(req.GetDirection().String()),
		ChannelName: req.GetChannelName(),
		Headers:     req.GetHeaders(),
		Body:        req.GetBody(),
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &orderv1.IngestWebhookResponse{
		Ok:              ev != nil,
		EventId:         ev.EventID,
		PaymentIntentId: ev.PaymentIntentID,
		ChargeId:        ev.ChargeID,
		RefundId:        ev.RefundID,
	}, nil
}

// ─── 转换 ─────────────────────────────────────────────────────────────────────

func piToProto(pi *domain.PaymentIntent) *orderv1.PaymentIntent {
	if pi == nil {
		return nil
	}
	p := &orderv1.PaymentIntent{
		Id:                  pi.ID,
		Amount:              pi.Amount,
		AmountSubtotal:      pi.AmountSubtotal,
		AmountCoupon:        pi.AmountCoupon,
		AmountPoints:        pi.AmountPoints,
		Currency:            pi.Currency,
		Status:              piStatusToProto(pi.Status),
		CustomerId:          pi.CustomerID,
		Description:         pi.Description,
		MchId:               pi.MchID,
		MchOrderNo:          pi.MchOrderNo,
		BusinessId:          pi.BusinessID,
		IdempotencyKey:      pi.IdempotencyKey,
		CaptureMethod:       captureMethodToProto(pi.CaptureMethod),
		ConfirmationMethod:  confirmationMethodToProto(pi.ConfirmationMethod),
		ClientSecret:        pi.ClientSecret,
		PaymentMethodTypes:  []string(pi.PaymentMethodTypes),
		PaymentMethod:       pi.PaymentMethod,
		ActiveChargeIds:     []string(pi.ActiveChargeIDs),
		ActiveRefundIds:     []string(pi.ActiveRefundIDs),
		AmountCapturable:    pi.AmountCapturable,
		AmountReceived:      pi.AmountReceived,
		ReturnUrl:           pi.ReturnURL,
		NotifyUrl:           pi.NotifyURL,
		StatementDescriptor: pi.StatementDescriptor,
		Metadata:            map[string]string(pi.Metadata),
		Livemode:            pi.Livemode,
		Created:             timestamppb.New(pi.Created),
		Updated:             timestamppb.New(pi.Updated),
	}
	if pi.CanceledAt != nil {
		p.CanceledAt = timestamppb.New(*pi.CanceledAt)
	}
	return p
}

func chargeToProto(c *domain.Charge) *orderv1.Charge {
	if c == nil {
		return nil
	}
	return &orderv1.Charge{
		Id:                 c.ID,
		PaymentIntentId:    c.PaymentIntentID,
		Amount:             c.Amount,
		AmountCaptured:     c.AmountCaptured,
		AmountRefunded:     c.AmountRefunded,
		Currency:           c.Currency,
		Status:             chargeStatusToProto(c.Status),
		PaymentMethod:      c.PaymentMethod,
		Captured:           c.Captured,
		Paid:               c.Paid,
		Refunded:           c.Refunded,
		FailureCode:        c.FailureCode,
		FailureMessage:     c.FailureMessage,
		ReceiptUrl:         c.ReceiptURL,
		BalanceTransaction: c.BalanceTransaction,
		Livemode:           c.Livemode,
		Metadata:           map[string]string(c.Metadata),
		Created:            timestamppb.New(c.Created),
		Updated:            timestamppb.New(c.Updated),
		Outcome: &orderv1.ChargeOutcome{
			RiskLevel:     c.OutcomeRiskLevel,
			RiskScore:     int32(c.OutcomeRiskScore),
			SellerMessage: c.OutcomeSellerMsg,
			Type:          c.OutcomeType,
			Reason:        c.OutcomeReason,
			NetworkStatus: c.OutcomeNetwork,
		},
	}
}

func chargesToProto(list []*domain.Charge) []*orderv1.Charge {
	out := make([]*orderv1.Charge, 0, len(list))
	for _, c := range list {
		out = append(out, chargeToProto(c))
	}
	return out
}

func refundToProto(r *domain.Refund) *orderv1.Refund {
	if r == nil {
		return nil
	}
	return &orderv1.Refund{
		Id:              r.ID,
		ChargeId:        r.ChargeID,
		PaymentIntentId: r.PaymentIntentID,
		Amount:          r.Amount,
		Currency:        r.Currency,
		Status:          refundStatusToProto(r.Status),
		Reason:          refundReasonToProto(r.Reason),
		FailureReason:   r.FailureReason,
		ReceiptNumber:   r.ReceiptNumber,
		Metadata:        map[string]string(r.Metadata),
		Created:         timestamppb.New(r.Created),
		Updated:         timestamppb.New(r.Updated),
	}
}

func payActionToProto(a *domain.PayAction) *orderv1.PayAction {
	if a == nil {
		return nil
	}
	p := &orderv1.PayAction{
		Id:              a.ID,
		PaymentIntentId: a.PaymentIntentID,
		ChargeId:        a.ChargeID,
		ActionType:      string(a.ActionType),
		Status:          payActionStatusToProto(a.Status),
		Payload:         map[string]string(a.Payload),
		AttemptCount:    int32(a.AttemptCount),
		MaxAttempts:     int32(a.MaxAttempts),
		FailureReason:   a.FailureReason,
		Created:         timestamppb.New(a.Created),
		Updated:         timestamppb.New(a.Updated),
	}
	if a.ExpiresAt != nil {
		p.ExpiresAt = timestamppb.New(*a.ExpiresAt)
	}
	if a.CompletedAt != nil {
		p.CompletedAt = timestamppb.New(*a.CompletedAt)
	}
	return p
}

func piStatusToProto(s domain.PaymentIntentStatus) orderv1.PaymentIntentStatus {
	switch s {
	case domain.PIStatusCreated:
		return orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_CREATED
	case domain.PIStatusRequiresAction:
		return orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_REQUIRES_ACTION
	case domain.PIStatusProcessing:
		return orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_PROCESSING
	case domain.PIStatusSucceeded:
		return orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_SUCCEEDED
	case domain.PIStatusFailed:
		return orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_FAILED
	case domain.PIStatusCanceled:
		return orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_CANCELED
	}
	return orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_UNSPECIFIED
}

func captureMethodFromProto(p orderv1.CaptureMethod) domain.CaptureMethod {
	switch p {
	case orderv1.CaptureMethod_CAPTURE_METHOD_MANUAL:
		return domain.CaptureManual
	case orderv1.CaptureMethod_CAPTURE_METHOD_AUTOMATIC:
		return domain.CaptureAutomatic
	}
	return ""
}

func captureMethodToProto(m domain.CaptureMethod) orderv1.CaptureMethod {
	switch m {
	case domain.CaptureAutomatic:
		return orderv1.CaptureMethod_CAPTURE_METHOD_AUTOMATIC
	case domain.CaptureManual:
		return orderv1.CaptureMethod_CAPTURE_METHOD_MANUAL
	}
	return orderv1.CaptureMethod_CAPTURE_METHOD_UNSPECIFIED
}

func confirmationMethodFromProto(p orderv1.ConfirmationMethod) domain.ConfirmationMethod {
	switch p {
	case orderv1.ConfirmationMethod_CONFIRMATION_METHOD_MANUAL:
		return domain.ConfirmationManual
	case orderv1.ConfirmationMethod_CONFIRMATION_METHOD_AUTOMATIC:
		return domain.ConfirmationAutomatic
	}
	return ""
}

func confirmationMethodToProto(m domain.ConfirmationMethod) orderv1.ConfirmationMethod {
	switch m {
	case domain.ConfirmationAutomatic:
		return orderv1.ConfirmationMethod_CONFIRMATION_METHOD_AUTOMATIC
	case domain.ConfirmationManual:
		return orderv1.ConfirmationMethod_CONFIRMATION_METHOD_MANUAL
	}
	return orderv1.ConfirmationMethod_CONFIRMATION_METHOD_UNSPECIFIED
}

func chargeStatusToProto(s domain.ChargeStatus) orderv1.ChargeStatus {
	switch s {
	case domain.ChargeStatusPending:
		return orderv1.ChargeStatus_CHARGE_STATUS_PENDING
	case domain.ChargeStatusSucceeded:
		return orderv1.ChargeStatus_CHARGE_STATUS_SUCCEEDED
	case domain.ChargeStatusFailed:
		return orderv1.ChargeStatus_CHARGE_STATUS_FAILED
	}
	return orderv1.ChargeStatus_CHARGE_STATUS_UNSPECIFIED
}

func refundStatusToProto(s domain.RefundStatus) orderv1.RefundStatus {
	switch s {
	case domain.RefundStatusPending:
		return orderv1.RefundStatus_REFUND_STATUS_PENDING
	case domain.RefundStatusSucceeded:
		return orderv1.RefundStatus_REFUND_STATUS_SUCCEEDED
	case domain.RefundStatusFailed:
		return orderv1.RefundStatus_REFUND_STATUS_FAILED
	}
	return orderv1.RefundStatus_REFUND_STATUS_UNSPECIFIED
}

func payActionStatusToProto(s domain.PayActionStatus) orderv1.PayActionStatus {
	switch s {
	case domain.PayActionStatusPending:
		return orderv1.PayActionStatus_PAY_ACTION_STATUS_PENDING
	case domain.PayActionStatusSucceeded:
		return orderv1.PayActionStatus_PAY_ACTION_STATUS_SUCCEEDED
	case domain.PayActionStatusFailed:
		return orderv1.PayActionStatus_PAY_ACTION_STATUS_FAILED
	case domain.PayActionStatusExpired:
		return orderv1.PayActionStatus_PAY_ACTION_STATUS_EXPIRED
	}
	return orderv1.PayActionStatus_PAY_ACTION_STATUS_UNSPECIFIED
}

func refundReasonFromProto(p orderv1.RefundReason) domain.RefundReason {
	switch p {
	case orderv1.RefundReason_REFUND_REASON_DUPLICATE:
		return domain.RefundReasonDuplicate
	case orderv1.RefundReason_REFUND_REASON_FRAUDULENT:
		return domain.RefundReasonFraudulent
	case orderv1.RefundReason_REFUND_REASON_REQUESTED_BY_CUSTOMER:
		return domain.RefundReasonRequestedByCustomer
	case orderv1.RefundReason_REFUND_REASON_EXPIRED_UNCAPTURED:
		return domain.RefundReasonExpiredUncaptured
	}
	return ""
}

func refundReasonToProto(r domain.RefundReason) orderv1.RefundReason {
	switch r {
	case domain.RefundReasonDuplicate:
		return orderv1.RefundReason_REFUND_REASON_DUPLICATE
	case domain.RefundReasonFraudulent:
		return orderv1.RefundReason_REFUND_REASON_FRAUDULENT
	case domain.RefundReasonRequestedByCustomer:
		return orderv1.RefundReason_REFUND_REASON_REQUESTED_BY_CUSTOMER
	case domain.RefundReasonExpiredUncaptured:
		return orderv1.RefundReason_REFUND_REASON_EXPIRED_UNCAPTURED
	}
	return orderv1.RefundReason_REFUND_REASON_UNSPECIFIED
}

func mapError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrPaymentIntentNotFound),
		errors.Is(err, domain.ErrChargeNotFound),
		errors.Is(err, domain.ErrRefundNotFound),
		errors.Is(err, domain.ErrPayActionNotFound):
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, domain.ErrValidation):
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrPayActionNotPending):
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, domain.ErrRefundAmountExceeded),
		errors.Is(err, domain.ErrPayActionTooManyAttempts):
		return fmt.Errorf("%s", err.Error())
	default:
		return fmt.Errorf("internal error")
	}
}
