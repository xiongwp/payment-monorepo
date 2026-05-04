package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"

	orderv1 "github.com/xiongwp/order-core/api/proto/order/v1"
	"github.com/xiongwp/order-core/internal/repo"
	"github.com/xiongwp/order-core/internal/webhook"
)

// WebhookDeliveryServer exposes list / retry / test-send on webhook_deliveries
// so the admin UI can audit + manually kick stuck webhooks.
type WebhookDeliveryServer struct {
	orderv1.UnimplementedWebhookDeliveryServiceServer
	mgr    *repo.Manager
	disp   *webhook.Dispatcher
}

// NewWebhookDeliveryServer constructs the adapter.
func NewWebhookDeliveryServer(mgr *repo.Manager, d *webhook.Dispatcher) *WebhookDeliveryServer {
	return &WebhookDeliveryServer{mgr: mgr, disp: d}
}

func (s *WebhookDeliveryServer) List(ctx context.Context, req *orderv1.ListWebhookDeliveriesRequest) (*orderv1.ListWebhookDeliveriesResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := s.mgr.GetMeta().WithContext(ctx).Table("webhook_deliveries")
	if req.GetMerchantId() != "" {
		q = q.Where("merchant_id = ?", req.GetMerchantId())
	}
	if req.GetStatus() != "" {
		q = q.Where("status = ?", req.GetStatus())
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var rows []webhook.Delivery
	if err := q.Order("id DESC").Limit(limit).Offset(int(req.GetOffset())).Find(&rows).Error; err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &orderv1.ListWebhookDeliveriesResponse{Total: total, Items: make([]*orderv1.WebhookDelivery, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, pbWebhookDelivery(&r))
	}
	return out, nil
}

func (s *WebhookDeliveryServer) Retry(ctx context.Context, req *orderv1.RetryWebhookDeliveryRequest) (*orderv1.RetryWebhookDeliveryResponse, error) {
	// Mark the row as retry-ready: reset next_retry_at to now, bump back into
	// pending/failed so the worker picks it up. We keep attempts counter
	// truthful — manual retries also consume attempts. Ops can reset attempts
	// via DB if needed.
	res := s.mgr.GetMeta().WithContext(ctx).Table("webhook_deliveries").
		Where("id = ?", req.GetId()).
		Updates(map[string]any{
			"status":        "pending",
			"next_retry_at": time.Now(),
		})
	if res.Error != nil {
		return nil, status.Error(codes.Internal, res.Error.Error())
	}
	if res.RowsAffected == 0 {
		return nil, status.Error(codes.NotFound, "delivery not found")
	}
	var d webhook.Delivery
	if err := s.mgr.GetMeta().WithContext(ctx).Table("webhook_deliveries").
		Where("id = ?", req.GetId()).First(&d).Error; err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &orderv1.RetryWebhookDeliveryResponse{Delivery: pbWebhookDelivery(&d)}, nil
}

func (s *WebhookDeliveryServer) TestSend(ctx context.Context, req *orderv1.TestWebhookDeliveryRequest) (*orderv1.TestWebhookDeliveryResponse, error) {
	// Look up the merchant's webhook_url + webhook_secret from merchants table.
	var mch struct {
		WebhookURL    string `gorm:"column:webhook_url"`
		WebhookSecret string `gorm:"column:webhook_secret"`
	}
	if err := s.mgr.GetMeta().WithContext(ctx).Table("merchants").
		Select("webhook_url, webhook_secret").
		Where("id = ?", req.GetMerchantId()).
		Scan(&mch).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, status.Error(codes.NotFound, "merchant not found")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	if mch.WebhookURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "merchant has no webhook_url configured")
	}
	evtType := req.GetEventType()
	if evtType == "" {
		evtType = "webhook.test"
	}
	var payload any
	if p := req.GetPayload(); p != "" {
		if err := json.Unmarshal([]byte(p), &payload); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "payload not valid JSON: %v", err)
		}
	} else {
		payload = map[string]any{"message": "this is a test webhook from order-core admin"}
	}
	evt := webhook.Event{
		ID:         fmt.Sprintf("evt_test_%d", time.Now().UnixNano()),
		MerchantID: req.GetMerchantId(),
		Type:       evtType,
		Payload:    payload,
	}
	if err := s.disp.Enqueue(ctx, mch.WebhookURL, mch.WebhookSecret, evt); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// Return the row we just created
	var d webhook.Delivery
	if err := s.mgr.GetMeta().WithContext(ctx).Table("webhook_deliveries").
		Where("event_id = ?", evt.ID).First(&d).Error; err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &orderv1.TestWebhookDeliveryResponse{Delivery: pbWebhookDelivery(&d)}, nil
}

func pbWebhookDelivery(d *webhook.Delivery) *orderv1.WebhookDelivery {
	return &orderv1.WebhookDelivery{
		Id:          d.ID,
		MerchantId:  d.MerchantID,
		EventId:     d.EventID,
		EventType:   d.EventType,
		Payload:     d.Payload,
		Url:         d.URL,
		Status:      d.Status,
		HttpStatus:  int32(d.HTTPStatus),
		Attempts:    int32(d.Attempts),
		MaxAttempts: int32(d.MaxAttempts),
		LastError:   d.LastError,
		NextRetryMs: tsMsPtr(d.NextRetryAt),
		CreatedMs:   tsMs(d.CreatedAt),
		UpdatedMs:   tsMs(d.UpdatedAt),
	}
}

func tsMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func tsMsPtr(t *time.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
