package server

import (
	"context"
	"fmt"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/service"
	"github.com/xiongwp/user-merchant-core/pkg/validatex"
)

// MerchantServer adapts service.MerchantService onto the gRPC MerchantService.
type MerchantServer struct {
	usermerchantv1.UnimplementedMerchantServiceServer
	svc service.MerchantService
}

// NewMerchantServer constructs the gRPC adapter.
func NewMerchantServer(svc service.MerchantService) *MerchantServer {
	return &MerchantServer{svc: svc}
}

// ─── conversions ─────────────────────────────────────────────────────────────

func pbMerchant(m *domain.Merchant) *usermerchantv1.Merchant {
	if m == nil {
		return nil
	}
	out := &usermerchantv1.Merchant{
		Id:             m.ID,
		Name:           m.Name,
		LegalName:      m.LegalName,
		Country:        m.Country,
		BusinessType:   m.BusinessType,
		TaxId:          m.TaxID,
		ContactEmail:   m.ContactEmail,
		ContactPhone:   m.ContactPhone,
		Website:        m.Website,
		Mcc:            m.MCC,
		WebhookUrl:     m.WebhookURL,
		KycStatus:      pbKycStatus(m.KYCStatus),
		KycLevel:       int32(m.KYCLevel),
		KycReason:      m.KYCReason,
		KycReviewer:    m.KYCReviewer,
		RiskTier:       m.RiskTier,
		RateLimitRps:   int32(m.RateLimitRPS),
		SettleCurrency: m.SettleCurrency,
		SettleMethod:   m.SettleMethod,
		SettleAccount:  m.SettleAccount,
		SettleBank:     m.SettleBank,
		SettleHolder:   m.SettleHolder,
		Status:         pbMerchantStatus(m.Status),
		CreatedMs:      m.Created.UnixMilli(),
		UpdatedMs:      m.Updated.UnixMilli(),
	}
	if m.KYCReviewedAt != nil {
		out.KycReviewedAtMs = m.KYCReviewedAt.UnixMilli()
	}
	if len(m.Metadata) > 0 {
		out.Metadata = make(map[string]string, len(m.Metadata))
		for k, v := range m.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

func pbKycStatus(s domain.KYCStatus) usermerchantv1.KycStatus {
	switch s {
	case domain.KYCStatusPending:
		return usermerchantv1.KycStatus_KYC_STATUS_PENDING
	case domain.KYCStatusSubmitted:
		return usermerchantv1.KycStatus_KYC_STATUS_SUBMITTED
	case domain.KYCStatusReviewing:
		return usermerchantv1.KycStatus_KYC_STATUS_REVIEWING
	case domain.KYCStatusNeedsMoreInfo:
		return usermerchantv1.KycStatus_KYC_STATUS_NEEDS_MORE_INFO
	case domain.KYCStatusApproved:
		return usermerchantv1.KycStatus_KYC_STATUS_APPROVED
	case domain.KYCStatusRejected:
		return usermerchantv1.KycStatus_KYC_STATUS_REJECTED
	case domain.KYCStatusSuspended:
		return usermerchantv1.KycStatus_KYC_STATUS_SUSPENDED
	case domain.KYCStatusTerminated:
		return usermerchantv1.KycStatus_KYC_STATUS_TERMINATED
	}
	return usermerchantv1.KycStatus_KYC_STATUS_UNSPECIFIED
}

func domainKycStatus(s usermerchantv1.KycStatus) domain.KYCStatus {
	switch s {
	case usermerchantv1.KycStatus_KYC_STATUS_PENDING:
		return domain.KYCStatusPending
	case usermerchantv1.KycStatus_KYC_STATUS_SUBMITTED:
		return domain.KYCStatusSubmitted
	case usermerchantv1.KycStatus_KYC_STATUS_REVIEWING:
		return domain.KYCStatusReviewing
	case usermerchantv1.KycStatus_KYC_STATUS_NEEDS_MORE_INFO:
		return domain.KYCStatusNeedsMoreInfo
	case usermerchantv1.KycStatus_KYC_STATUS_APPROVED:
		return domain.KYCStatusApproved
	case usermerchantv1.KycStatus_KYC_STATUS_REJECTED:
		return domain.KYCStatusRejected
	case usermerchantv1.KycStatus_KYC_STATUS_SUSPENDED:
		return domain.KYCStatusSuspended
	case usermerchantv1.KycStatus_KYC_STATUS_TERMINATED:
		return domain.KYCStatusTerminated
	}
	return ""
}

func pbMerchantStatus(s domain.MerchantStatus) usermerchantv1.MerchantStatus {
	switch s {
	case domain.MerchantStatusPending:
		return usermerchantv1.MerchantStatus_MERCHANT_STATUS_PENDING
	case domain.MerchantStatusActive:
		return usermerchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE
	case domain.MerchantStatusSuspended:
		return usermerchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED
	case domain.MerchantStatusTerminated:
		return usermerchantv1.MerchantStatus_MERCHANT_STATUS_TERMINATED
	}
	return usermerchantv1.MerchantStatus_MERCHANT_STATUS_UNSPECIFIED
}

func domainMerchantStatus(s usermerchantv1.MerchantStatus) domain.MerchantStatus {
	switch s {
	case usermerchantv1.MerchantStatus_MERCHANT_STATUS_PENDING:
		return domain.MerchantStatusPending
	case usermerchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE:
		return domain.MerchantStatusActive
	case usermerchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED:
		return domain.MerchantStatusSuspended
	case usermerchantv1.MerchantStatus_MERCHANT_STATUS_TERMINATED:
		return domain.MerchantStatusTerminated
	}
	return ""
}

func pbDoc(d *domain.MerchantKYCDocument) *usermerchantv1.MerchantKycDocument {
	if d == nil {
		return nil
	}
	out := &usermerchantv1.MerchantKycDocument{
		Id:           d.ID,
		MerchantId:   d.MerchantID,
		DocType:      d.DocType,
		DocNumber:    d.DocNumber,
		FileUrl:      d.FileURL,
		MimeType:     d.MimeType,
		SizeBytes:    int32(d.SizeBytes),
		UploadedBy:   d.UploadedBy,
		ReviewStatus: d.ReviewStatus,
		ReviewNote:   d.ReviewNote,
		CreatedMs:    d.Created.UnixMilli(),
		UpdatedMs:    d.Updated.UnixMilli(),
	}
	if d.ExpiresAt != nil {
		out.ExpiresAtMs = d.ExpiresAt.UnixMilli()
	}
	return out
}

func pbAudit(a *domain.MerchantKYCAudit) *usermerchantv1.MerchantKycAudit {
	return &usermerchantv1.MerchantKycAudit{
		Id:         a.ID,
		MerchantId: a.MerchantID,
		FromStatus: pbKycStatus(a.FromStatus),
		ToStatus:   pbKycStatus(a.ToStatus),
		Reason:     a.Reason,
		Actor:      a.Actor,
		CreatedMs:  a.Created.UnixMilli(),
	}
}

// ─── RPC handlers ────────────────────────────────────────────────────────────

func (m *MerchantServer) Create(ctx context.Context, req *usermerchantv1.CreateMerchantRequest) (*usermerchantv1.CreateMerchantResponse, error) {
	// 入参校验：handler 边界就拦下明显非法输入，不让 service/DB 白跑一趟。
	// 错误用 domain.ErrValidation 包装，errx 自动映射到 InvalidArgument。
	if err := validatex.All(
		validatex.Required("name", req.GetName()),
		validatex.MaxLen("name", req.GetName(), 128),
		validatex.MaxLen("legal_name", req.GetLegalName(), 256),
		validatex.Required("contact_email", req.GetContactEmail()),
		validatex.Email("contact_email", req.GetContactEmail()),
		validatex.MaxLen("contact_email", req.GetContactEmail(), 128),
		validatex.Phone("contact_phone", req.GetContactPhone()),
		validatex.MaxLen("contact_phone", req.GetContactPhone(), 32),
		validatex.CountryISO2("country", req.GetCountry()),
		validatex.CurrencyISO4217("settle_currency", req.GetSettleCurrency()),
		validatex.HTTPURL("website", req.GetWebsite()),
		validatex.HTTPURL("webhook_url", req.GetWebhookUrl()),
		validatex.OneOf("business_type", req.GetBusinessType(),
			"", "individual", "corporate", "non_profit", "government"),
	); err != nil {
		return nil, grpcErr(fmt.Errorf("%w: %s", domain.ErrValidation, err.Error()))
	}
	md := make(domain.Metadata, len(req.GetMetadata()))
	for k, v := range req.GetMetadata() {
		md[k] = v
	}
	out, err := m.svc.Create(ctx, &service.CreateMerchantInput{
		Name:           req.GetName(),
		LegalName:      req.GetLegalName(),
		Country:        req.GetCountry(),
		BusinessType:   req.GetBusinessType(),
		TaxID:          req.GetTaxId(),
		ContactEmail:   req.GetContactEmail(),
		ContactPhone:   req.GetContactPhone(),
		Website:        req.GetWebsite(),
		MCC:            req.GetMcc(),
		WebhookURL:     req.GetWebhookUrl(),
		SettleCurrency: req.GetSettleCurrency(),
		SettleMethod:   req.GetSettleMethod(),
		SettleAccount:  req.GetSettleAccount(),
		SettleBank:     req.GetSettleBank(),
		SettleHolder:   req.GetSettleHolder(),
		Metadata:       md,
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.CreateMerchantResponse{
		Merchant:      pbMerchant(out.Merchant),
		LiveSecretKey: out.LiveSecretKey,
		TestSecretKey: out.TestSecretKey,
		WebhookSecret: out.WebhookSecret,
	}, nil
}

func (m *MerchantServer) BatchGet(ctx context.Context, req *usermerchantv1.BatchGetMerchantsRequest) (*usermerchantv1.BatchGetMerchantsResponse, error) {
	got, err := m.svc.BatchGet(ctx, req.GetIds())
	if err != nil {
		return nil, grpcErr(err)
	}
	out := &usermerchantv1.BatchGetMerchantsResponse{
		Merchants: make(map[string]*usermerchantv1.Merchant, len(got)),
	}
	for id, m := range got {
		out.Merchants[id] = pbMerchant(m)
	}
	return out, nil
}

func (m *MerchantServer) Get(ctx context.Context, req *usermerchantv1.GetMerchantRequest) (*usermerchantv1.GetMerchantResponse, error) {
	got, err := m.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.GetMerchantResponse{Merchant: pbMerchant(got)}, nil
}

func (m *MerchantServer) List(ctx context.Context, req *usermerchantv1.ListMerchantsRequest) (*usermerchantv1.ListMerchantsResponse, error) {
	ms, total, err := m.svc.List(ctx,
		domainMerchantStatus(req.GetStatus()),
		domainKycStatus(req.GetKycStatus()),
		int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, grpcErr(err)
	}
	out := &usermerchantv1.ListMerchantsResponse{Total: total, Merchants: make([]*usermerchantv1.Merchant, 0, len(ms))}
	for _, it := range ms {
		out.Merchants = append(out.Merchants, pbMerchant(it))
	}
	return out, nil
}

func (m *MerchantServer) Update(ctx context.Context, req *usermerchantv1.UpdateMerchantRequest) (*usermerchantv1.UpdateMerchantResponse, error) {
	fields := make(map[string]any, len(req.GetFields()))
	for k, v := range req.GetFields() {
		fields[k] = v
	}
	got, err := m.svc.Update(ctx, req.GetId(), fields)
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.UpdateMerchantResponse{Merchant: pbMerchant(got)}, nil
}

func (m *MerchantServer) RotateApiKey(ctx context.Context, req *usermerchantv1.RotateApiKeyRequest) (*usermerchantv1.RotateApiKeyResponse, error) {
	if err := validatex.All(
		validatex.Required("id", req.GetId()),
		validatex.OneOf("kind", req.GetKind(), "live", "test"),
	); err != nil {
		return nil, grpcErr(fmt.Errorf("%w: %s", domain.ErrValidation, err.Error()))
	}
	plain, err := m.svc.RotateAPIKeys(ctx, req.GetId(), req.GetKind())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.RotateApiKeyResponse{Plaintext: plain}, nil
}

// KYC transitions — each RPC delegates to the matching service method.

func (m *MerchantServer) SubmitKyc(ctx context.Context, req *usermerchantv1.KycTransitionRequest) (*usermerchantv1.KycTransitionResponse, error) {
	out, err := m.svc.SubmitKYC(ctx, req.GetId(), req.GetActor())
	return kycResp(out, err)
}
func (m *MerchantServer) StartReview(ctx context.Context, req *usermerchantv1.KycTransitionRequest) (*usermerchantv1.KycTransitionResponse, error) {
	out, err := m.svc.StartReview(ctx, req.GetId(), req.GetActor())
	return kycResp(out, err)
}
func (m *MerchantServer) Approve(ctx context.Context, req *usermerchantv1.KycTransitionRequest) (*usermerchantv1.KycTransitionResponse, error) {
	out, err := m.svc.Approve(ctx, req.GetId(), req.GetActor())
	return kycResp(out, err)
}
func (m *MerchantServer) Reject(ctx context.Context, req *usermerchantv1.KycTransitionRequest) (*usermerchantv1.KycTransitionResponse, error) {
	out, err := m.svc.Reject(ctx, req.GetId(), req.GetActor(), req.GetReason())
	return kycResp(out, err)
}
func (m *MerchantServer) RequestMoreInfo(ctx context.Context, req *usermerchantv1.KycTransitionRequest) (*usermerchantv1.KycTransitionResponse, error) {
	out, err := m.svc.RequestMoreInfo(ctx, req.GetId(), req.GetActor(), req.GetReason())
	return kycResp(out, err)
}
func (m *MerchantServer) Suspend(ctx context.Context, req *usermerchantv1.KycTransitionRequest) (*usermerchantv1.KycTransitionResponse, error) {
	out, err := m.svc.Suspend(ctx, req.GetId(), req.GetActor(), req.GetReason())
	return kycResp(out, err)
}
func (m *MerchantServer) Unsuspend(ctx context.Context, req *usermerchantv1.KycTransitionRequest) (*usermerchantv1.KycTransitionResponse, error) {
	out, err := m.svc.Unsuspend(ctx, req.GetId(), req.GetActor())
	return kycResp(out, err)
}
func (m *MerchantServer) Terminate(ctx context.Context, req *usermerchantv1.KycTransitionRequest) (*usermerchantv1.KycTransitionResponse, error) {
	out, err := m.svc.Terminate(ctx, req.GetId(), req.GetActor(), req.GetReason())
	return kycResp(out, err)
}

func kycResp(m *domain.Merchant, err error) (*usermerchantv1.KycTransitionResponse, error) {
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.KycTransitionResponse{Merchant: pbMerchant(m)}, nil
}

// Documents

func (m *MerchantServer) AddDocument(ctx context.Context, req *usermerchantv1.AddKycDocumentRequest) (*usermerchantv1.AddKycDocumentResponse, error) {
	if err := validatex.All(
		validatex.Required("merchant_id", req.GetMerchantId()),
		validatex.Required("doc_type", req.GetDocType()),
		validatex.Required("file_url", req.GetFileUrl()),
		validatex.HTTPURL("file_url", req.GetFileUrl()),
		validatex.MaxLen("doc_number", req.GetDocNumber(), 128),
		validatex.Int64Range("size_bytes", int64(req.GetSizeBytes()), 0, 50*1024*1024),
	); err != nil {
		return nil, grpcErr(fmt.Errorf("%w: %s", domain.ErrValidation, err.Error()))
	}
	d, err := m.svc.AddDocument(ctx, &domain.MerchantKYCDocument{
		MerchantID: req.GetMerchantId(),
		DocType:    req.GetDocType(),
		DocNumber:  req.GetDocNumber(),
		FileURL:    req.GetFileUrl(),
		MimeType:   req.GetMimeType(),
		SizeBytes:  int(req.GetSizeBytes()),
		UploadedBy: req.GetUploadedBy(),
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.AddKycDocumentResponse{Document: pbDoc(d)}, nil
}

func (m *MerchantServer) ListDocuments(ctx context.Context, req *usermerchantv1.ListKycDocumentsRequest) (*usermerchantv1.ListKycDocumentsResponse, error) {
	docs, err := m.svc.ListDocuments(ctx, req.GetMerchantId())
	if err != nil {
		return nil, grpcErr(err)
	}
	out := &usermerchantv1.ListKycDocumentsResponse{Documents: make([]*usermerchantv1.MerchantKycDocument, 0, len(docs))}
	for _, d := range docs {
		out.Documents = append(out.Documents, pbDoc(d))
	}
	return out, nil
}

func (m *MerchantServer) ReviewDocument(ctx context.Context, req *usermerchantv1.ReviewKycDocumentRequest) (*usermerchantv1.ReviewKycDocumentResponse, error) {
	if err := m.svc.ReviewDocument(ctx, req.GetId(), req.GetStatus(), req.GetNote()); err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.ReviewKycDocumentResponse{}, nil
}

func (m *MerchantServer) ListAudits(ctx context.Context, req *usermerchantv1.ListKycAuditsRequest) (*usermerchantv1.ListKycAuditsResponse, error) {
	audits, err := m.svc.ListKYCAudits(ctx, req.GetMerchantId(), int(req.GetLimit()))
	if err != nil {
		return nil, grpcErr(err)
	}
	out := &usermerchantv1.ListKycAuditsResponse{Audits: make([]*usermerchantv1.MerchantKycAudit, 0, len(audits))}
	for _, a := range audits {
		out.Audits = append(out.Audits, pbAudit(a))
	}
	return out, nil
}

