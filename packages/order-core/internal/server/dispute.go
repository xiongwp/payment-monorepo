package server

import (
	"context"
	"errors"
	"time"
	orderv1 "reconcile-system/packages/order-core/kitex_gen/order/v1"
	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/service"
)

// DisputeServer adapts service.DisputeService onto the gRPC surface.
type DisputeServer struct {
	svc service.DisputeService
}

// NewDisputeServer constructs the adapter.
func NewDisputeServer(s service.DisputeService) *DisputeServer { return &DisputeServer{svc: s} }

// ─── conversions ─────────────────────────────────────────────────────────────

func pbDispute(d *domain.Dispute) *orderv1.Dispute {
	if d == nil {
		return nil
	}
	out := &orderv1.Dispute{
		Id:               d.ID,
		PaymentIntentId:  d.PaymentIntentID,
		ChargeId:         d.ChargeID,
		MerchantId:       d.MerchantID,
		Channel:          d.Channel,
		ChannelDisputeId: d.ChannelDisputeID,
		Status:           pbDisputeStatus(d.Status),
		Amount:           d.Amount,
		Currency:         d.Currency,
		Reason:           string(d.Reason),
		ReasonDetail:     d.ReasonDetail,
		AutoRefundCharge: d.AutoRefundCharge,
		CreatedMs:        d.Created.UnixMilli(),
		UpdatedMs:        d.Updated.UnixMilli(),
	}
	if d.EvidenceDueAt != nil {
		out.EvidenceDueAtMs = d.EvidenceDueAt.UnixMilli()
	}
	if d.DecidedAt != nil {
		out.DecidedAtMs = d.DecidedAt.UnixMilli()
	}
	if d.OutcomeAmount != nil {
		out.OutcomeAmount = *d.OutcomeAmount
	}
	if len(d.Evidence) > 0 {
		out.Evidence = make(map[string]string, len(d.Evidence))
		for k, v := range d.Evidence {
			out.Evidence[k] = v
		}
	}
	if len(d.Metadata) > 0 {
		out.Metadata = make(map[string]string, len(d.Metadata))
		for k, v := range d.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

func pbDisputeStatus(s domain.DisputeStatus) orderv1.DisputeStatus {
	switch s {
	case domain.DisputeNeedsResponse:
		return orderv1.DisputeStatus_DISPUTE_STATUS_NEEDS_RESPONSE
	case domain.DisputeUnderReview:
		return orderv1.DisputeStatus_DISPUTE_STATUS_UNDER_REVIEW
	case domain.DisputeWon:
		return orderv1.DisputeStatus_DISPUTE_STATUS_WON
	case domain.DisputeLost:
		return orderv1.DisputeStatus_DISPUTE_STATUS_LOST
	case domain.DisputeWarningClosed:
		return orderv1.DisputeStatus_DISPUTE_STATUS_WARNING_CLOSED
	case domain.DisputeChargeRefunded:
		return orderv1.DisputeStatus_DISPUTE_STATUS_CHARGE_REFUNDED
	case domain.DisputeCanceled:
		return orderv1.DisputeStatus_DISPUTE_STATUS_CANCELED
	}
	return orderv1.DisputeStatus_DISPUTE_STATUS_UNSPECIFIED
}

func domainDisputeStatus(s orderv1.DisputeStatus) domain.DisputeStatus {
	switch s {
	case orderv1.DisputeStatus_DISPUTE_STATUS_NEEDS_RESPONSE:
		return domain.DisputeNeedsResponse
	case orderv1.DisputeStatus_DISPUTE_STATUS_UNDER_REVIEW:
		return domain.DisputeUnderReview
	case orderv1.DisputeStatus_DISPUTE_STATUS_WON:
		return domain.DisputeWon
	case orderv1.DisputeStatus_DISPUTE_STATUS_LOST:
		return domain.DisputeLost
	case orderv1.DisputeStatus_DISPUTE_STATUS_WARNING_CLOSED:
		return domain.DisputeWarningClosed
	case orderv1.DisputeStatus_DISPUTE_STATUS_CHARGE_REFUNDED:
		return domain.DisputeChargeRefunded
	case orderv1.DisputeStatus_DISPUTE_STATUS_CANCELED:
		return domain.DisputeCanceled
	}
	return ""
}

func pbDisputeEvent(e *domain.DisputeEvent) *orderv1.DisputeEvent {
	out := &orderv1.DisputeEvent{
		Id:              e.ID,
		DisputeId:       e.DisputeID,
		PaymentIntentId: e.PaymentIntentID,
		FromStatus:      e.FromStatus,
		ToStatus:        e.ToStatus,
		Source:          e.Source,
		Actor:           e.Actor,
		Note:            e.Note,
		CreatedMs:       e.Created.UnixMilli(),
	}
	if len(e.Payload) > 0 {
		out.Payload = make(map[string]string, len(e.Payload))
		for k, v := range e.Payload {
			out.Payload[k] = v
		}
	}
	return out
}

// ─── RPC handlers ────────────────────────────────────────────────────────────

func (s *DisputeServer) Open(ctx context.Context, req *orderv1.OpenDisputeRequest) (*orderv1.OpenDisputeResponse, error) {
	var due *time.Time
	if v := req.GetEvidenceDueAtMs(); v > 0 {
		t := time.UnixMilli(v); due = &t
	}
	d, err := s.svc.Open(ctx, &service.OpenDisputeInput{
		PaymentIntentID:  req.GetPaymentIntentId(),
		ChargeID:         req.GetChargeId(),
		MerchantID:       req.GetMerchantId(),
		Channel:          req.GetChannel(),
		ChannelDisputeID: req.GetChannelDisputeId(),
		Amount:           req.GetAmount(),
		Currency:         req.GetCurrency(),
		Reason:           domain.DisputeReason(req.GetReason()),
		ReasonDetail:     req.GetReasonDetail(),
		EvidenceDueAt:    due,
		AutoRefundCharge: req.GetAutoRefundCharge(),
		Metadata:         copyStrMap(req.GetMetadata()),
		Source:           req.GetSource(),
	})
	if err != nil {
		return nil, disputeGrpcErr(err)
	}
	return &orderv1.OpenDisputeResponse{Dispute: pbDispute(d)}, nil
}

func (s *DisputeServer) Get(ctx context.Context, req *orderv1.GetDisputeRequest) (*orderv1.GetDisputeResponse, error) {
	d, err := s.svc.Get(ctx, req.GetPaymentIntentId(), req.GetId())
	if err != nil {
		return nil, disputeGrpcErr(err)
	}
	return &orderv1.GetDisputeResponse{Dispute: pbDispute(d)}, nil
}

func (s *DisputeServer) ListByPI(ctx context.Context, req *orderv1.ListDisputesByPIRequest) (*orderv1.ListDisputesByPIResponse, error) {
	list, err := s.svc.ListByPI(ctx, req.GetPaymentIntentId())
	if err != nil {
		return nil, disputeGrpcErr(err)
	}
	out := &orderv1.ListDisputesByPIResponse{Disputes: make([]*orderv1.Dispute, 0, len(list))}
	for _, d := range list {
		out.Disputes = append(out.Disputes, pbDispute(d))
	}
	return out, nil
}

func (s *DisputeServer) ListByMerchant(ctx context.Context, req *orderv1.ListDisputesByMerchantRequest) (*orderv1.ListDisputesByMerchantResponse, error) {
	list, total, err := s.svc.ListByMerchant(ctx, req.GetMerchantId(),
		domainDisputeStatus(req.GetStatus()),
		int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, disputeGrpcErr(err)
	}
	out := &orderv1.ListDisputesByMerchantResponse{Total: total, Disputes: make([]*orderv1.Dispute, 0, len(list))}
	for _, d := range list {
		out.Disputes = append(out.Disputes, pbDispute(d))
	}
	return out, nil
}

func (s *DisputeServer) ListEvents(ctx context.Context, req *orderv1.ListDisputeEventsRequest) (*orderv1.ListDisputeEventsResponse, error) {
	events, err := s.svc.ListEvents(ctx, req.GetPaymentIntentId(), req.GetDisputeId(), int(req.GetLimit()))
	if err != nil {
		return nil, disputeGrpcErr(err)
	}
	out := &orderv1.ListDisputeEventsResponse{Events: make([]*orderv1.DisputeEvent, 0, len(events))}
	for _, e := range events {
		out.Events = append(out.Events, pbDisputeEvent(e))
	}
	return out, nil
}

func (s *DisputeServer) SubmitEvidence(ctx context.Context, req *orderv1.SubmitEvidenceRequest) (*orderv1.SubmitEvidenceResponse, error) {
	d, err := s.svc.SubmitEvidence(ctx, req.GetPaymentIntentId(), req.GetId(), req.GetActor(), copyStrMap(req.GetEvidence()))
	if err != nil {
		return nil, disputeGrpcErr(err)
	}
	return &orderv1.SubmitEvidenceResponse{Dispute: pbDispute(d)}, nil
}

func (s *DisputeServer) Concede(ctx context.Context, req *orderv1.ConcedeDisputeRequest) (*orderv1.ConcedeDisputeResponse, error) {
	d, err := s.svc.Concede(ctx, req.GetPaymentIntentId(), req.GetId(), req.GetActor(), req.GetNote())
	if err != nil {
		return nil, disputeGrpcErr(err)
	}
	return &orderv1.ConcedeDisputeResponse{Dispute: pbDispute(d)}, nil
}

func (s *DisputeServer) Cancel(ctx context.Context, req *orderv1.CancelDisputeRequest) (*orderv1.CancelDisputeResponse, error) {
	d, err := s.svc.Cancel(ctx, req.GetPaymentIntentId(), req.GetId(), req.GetActor(), req.GetNote())
	if err != nil {
		return nil, disputeGrpcErr(err)
	}
	return &orderv1.CancelDisputeResponse{Dispute: pbDispute(d)}, nil
}

func (s *DisputeServer) MarkUnderReview(ctx context.Context, req *orderv1.MarkDisputeRequest) (*orderv1.MarkDisputeResponse, error) {
	return s.markResp(s.svc.MarkUnderReview(ctx, req.GetPaymentIntentId(), req.GetId(), copyStrMap(req.GetPayload())))
}
func (s *DisputeServer) MarkWon(ctx context.Context, req *orderv1.MarkDisputeRequest) (*orderv1.MarkDisputeResponse, error) {
	return s.markResp(s.svc.MarkWon(ctx, req.GetPaymentIntentId(), req.GetId(), copyStrMap(req.GetPayload())))
}
func (s *DisputeServer) MarkLost(ctx context.Context, req *orderv1.MarkDisputeRequest) (*orderv1.MarkDisputeResponse, error) {
	return s.markResp(s.svc.MarkLost(ctx, req.GetPaymentIntentId(), req.GetId(), req.GetOutcomeAmount(), copyStrMap(req.GetPayload())))
}
func (s *DisputeServer) MarkWarningClosed(ctx context.Context, req *orderv1.MarkDisputeRequest) (*orderv1.MarkDisputeResponse, error) {
	return s.markResp(s.svc.MarkWarningClosed(ctx, req.GetPaymentIntentId(), req.GetId(), copyStrMap(req.GetPayload())))
}

func (s *DisputeServer) markResp(d *domain.Dispute, err error) (*orderv1.MarkDisputeResponse, error) {
	if err != nil {
		return nil, disputeGrpcErr(err)
	}
	return &orderv1.MarkDisputeResponse{Dispute: pbDispute(d)}, nil
}

// disputeGrpcErr maps domain errors to gRPC codes.
func disputeGrpcErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrDisputeNotFound):
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, domain.ErrValidation):
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, domain.ErrDisputeInvalidTransition):
		return fmt.Errorf("%s", err.Error())
	}
	return fmt.Errorf("%s", err.Error())
}

func copyStrMap(in map[string]string) domain.Metadata {
	if len(in) == 0 {
		return nil
	}
	out := make(domain.Metadata, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
