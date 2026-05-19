package server

import (
	"context"
	"time"

	orderv1 "reconcile-system/packages/order-core/kitex_gen/order/v1"
	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/repo"
)

// AuditServer adapts repo.AdminAuditRepository onto the AuditService gRPC.
// No business logic here: audit is structurally simple (INSERT / SELECT), and
// introducing a service layer would just add a pass-through.
type AuditServer struct {
	repo repo.AdminAuditRepository
}

// NewAuditServer constructs the adapter.
func NewAuditServer(r repo.AdminAuditRepository) *AuditServer {
	return &AuditServer{repo: r}
}

func (s *AuditServer) Write(ctx context.Context, req *orderv1.WriteAuditRequest) (*orderv1.WriteAuditResponse, error) {
	entry := &domain.AdminAuditLog{
		Actor:        req.GetActor(),
		ActorIP:      req.GetActorIp(),
		Action:       req.GetAction(),
		TargetType:   req.GetTargetType(),
		TargetID:     req.GetTargetId(),
		HTTPMethod:   req.GetHttpMethod(),
		HTTPPath:     req.GetHttpPath(),
		HTTPStatus:   int(req.GetHttpStatus()),
		RequestBody:  req.GetRequestBody(),
		ResponseCode: req.GetResponseCode(),
		ResponseMsg:  req.GetResponseMsg(),
		DurationMs:   int(req.GetDurationMs()),
	}
	if err := s.repo.Insert(ctx, entry); err != nil {
		return nil, grpcErr(err)
	}
	return &orderv1.WriteAuditResponse{}, nil
}

func (s *AuditServer) List(ctx context.Context, req *orderv1.ListAuditRequest) (*orderv1.ListAuditResponse, error) {
	f := repo.AuditFilter{
		Actor:      req.GetActor(),
		Action:     req.GetAction(),
		TargetType: req.GetTargetType(),
		TargetID:   req.GetTargetId(),
		Limit:      int(req.GetLimit()),
		Offset:     int(req.GetOffset()),
	}
	if since := req.GetSinceMs(); since > 0 {
		t := time.UnixMilli(since)
		f.Since = &t
	}
	if until := req.GetUntilMs(); until > 0 {
		t := time.UnixMilli(until)
		f.Until = &t
	}
	rows, total, err := s.repo.List(ctx, f)
	if err != nil {
		return nil, grpcErr(err)
	}
	out := &orderv1.ListAuditResponse{Total: total, Items: make([]*orderv1.AuditEntry, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, &orderv1.AuditEntry{
			Id:           r.ID,
			Actor:        r.Actor,
			ActorIp:      r.ActorIP,
			Action:       r.Action,
			TargetType:   r.TargetType,
			TargetId:     r.TargetID,
			HttpMethod:   r.HTTPMethod,
			HttpPath:     r.HTTPPath,
			HttpStatus:   int32(r.HTTPStatus),
			RequestBody:  r.RequestBody,
			ResponseCode: r.ResponseCode,
			ResponseMsg:  r.ResponseMsg,
			DurationMs:   int32(r.DurationMs),
			CreatedMs:    r.Created.UnixMilli(),
		})
	}
	return out, nil
}
