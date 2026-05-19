package server

import (
	"context"

	usermerchantv1 "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1"
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/service"
)

// MerchantSecretServer gRPC adapter for merchant channel secret storage.
type MerchantSecretServer struct {
	svc service.MerchantSecretService
}

// NewMerchantSecretServer 构造
func NewMerchantSecretServer(s service.MerchantSecretService) *MerchantSecretServer {
	return &MerchantSecretServer{svc: s}
}

func pbSecret(s *domain.MerchantChannelSecret) *usermerchantv1.MerchantChannelSecret {
	return &usermerchantv1.MerchantChannelSecret{
		Id:         s.ID,
		MerchantId: s.MerchantID,
		Channel:    s.Channel,
		FieldName:  s.FieldName,
		MaskedHint: s.MaskedHint,
		Version:    int32(s.Version),
		CreatedBy:  s.CreatedBy,
		CreatedMs:  s.Created.UnixMilli(),
		UpdatedMs:  s.Updated.UnixMilli(),
	}
}

func (s *MerchantSecretServer) Put(ctx context.Context, req *usermerchantv1.PutMerchantSecretRequest) (*usermerchantv1.PutMerchantSecretResponse, error) {
	out, err := s.svc.Put(ctx, req.GetMerchantId(), req.GetChannel(), req.GetFieldName(), req.GetPlaintext(), req.GetActor())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.PutMerchantSecretResponse{Secret: pbSecret(out)}, nil
}

func (s *MerchantSecretServer) List(ctx context.Context, req *usermerchantv1.ListMerchantSecretsRequest) (*usermerchantv1.ListMerchantSecretsResponse, error) {
	var (
		rows []*domain.MerchantChannelSecret
		err  error
	)
	if req.GetChannel() != "" {
		rows, err = s.svc.ListForChannel(ctx, req.GetMerchantId(), req.GetChannel())
	} else {
		rows, err = s.svc.ListForMerchant(ctx, req.GetMerchantId())
	}
	if err != nil {
		return nil, grpcErr(err)
	}
	out := &usermerchantv1.ListMerchantSecretsResponse{Secrets: make([]*usermerchantv1.MerchantChannelSecret, 0, len(rows))}
	for _, r := range rows {
		out.Secrets = append(out.Secrets, pbSecret(r))
	}
	return out, nil
}

func (s *MerchantSecretServer) Delete(ctx context.Context, req *usermerchantv1.DeleteMerchantSecretRequest) (*usermerchantv1.DeleteMerchantSecretResponse, error) {
	if err := s.svc.Delete(ctx, req.GetMerchantId(), req.GetChannel(), req.GetFieldName()); err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.DeleteMerchantSecretResponse{}, nil
}

func (s *MerchantSecretServer) BulkGetPlaintext(ctx context.Context, req *usermerchantv1.BulkGetMerchantSecretsRequest) (*usermerchantv1.BulkGetMerchantSecretsResponse, error) {
	out, err := s.svc.BulkGetPlaintext(ctx, req.GetMerchantId(), req.GetChannel())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.BulkGetMerchantSecretsResponse{Fields: out}, nil
}
