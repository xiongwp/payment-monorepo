package server

import (
	"context"

	usermerchantv1 "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1"
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/repo"
)

// AuditServer 把 admin_audit_log 的只读查询暴露成 Kitex AuditService.
// 写入由 service 层在 mutation 路径完成 (auditstore.AuditStore).
// 老的 grpcutil.AuditInterceptor 已删.
type AuditServer struct {
	repo repo.AuditRepository
}

// NewAuditServer 构造
func NewAuditServer(r repo.AuditRepository) *AuditServer {
	return &AuditServer{repo: r}
}

func (s *AuditServer) List(ctx context.Context, req *usermerchantv1.ListAuditLogsRequest) (*usermerchantv1.ListAuditLogsResponse, error) {
	rows, err := s.repo.List(ctx, req.GetActor(), req.GetTarget(), int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, grpcErr(err)
	}
	out := &usermerchantv1.ListAuditLogsResponse{
		Entries: make([]*usermerchantv1.AuditLogEntry, 0, len(rows)),
	}
	for _, r := range rows {
		out.Entries = append(out.Entries, pbAuditLog(r))
	}
	return out, nil
}

func pbAuditLog(a *domain.AdminAuditLog) *usermerchantv1.AuditLogEntry {
	return &usermerchantv1.AuditLogEntry{
		Id:          a.ID,
		Actor:       a.Actor,
		ActorIp:     a.ActorIP,
		Method:      a.Method,
		TargetId:    a.TargetID,
		RequestBody: a.RequestBody,
		StatusCode:  a.StatusCode,
		ResponseErr: a.ResponseErr,
		DurationMs:  int32(a.DurationMs),
		TraceId:     a.TraceID,
		PrevHash:    a.PrevHash,
		RowHash:     a.RowHash,
		CreatedMs:   a.Created.UnixMilli(),
	}
}
