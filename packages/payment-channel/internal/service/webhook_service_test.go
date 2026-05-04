package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
)

type captureForwarder struct {
	calls atomic.Int32
	last  *channel.WebhookEvent
	err   error
}

func (f *captureForwarder) Forward(_ context.Context, _ string, evt *channel.WebhookEvent) error {
	f.calls.Add(1)
	f.last = evt
	return f.err
}

func TestWebhook_Ingest_HappyPath(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.webhookEvent = &channel.WebhookEvent{
		EventID: "evt_1", EventType: "charge.succeeded", PiID: "pi_1", ExternalRefNo: "ref_1",
	}
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{}
	svc := NewWebhookService(newFakeRegistry(ad), whRepo, fwd, zap.NewNop())

	err := svc.Ingest(context.Background(), "gcash", map[string]string{"X-Sig": "ok"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest err: %v", err)
	}
	if got := fwd.calls.Load(); got != 1 {
		t.Fatalf("want 1 forward got %d", got)
	}
	if fwd.last.EventType != "charge.succeeded" {
		t.Fatalf("wrong event forwarded: %+v", fwd.last)
	}
	if whRepo.count() != 1 {
		t.Fatalf("want 1 webhook_raw row got %d", whRepo.count())
	}
}

func TestWebhook_Ingest_Dedup(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.webhookEvent = &channel.WebhookEvent{EventID: "dup", EventType: "charge.succeeded", PiID: "pi_1"}
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{}
	svc := NewWebhookService(newFakeRegistry(ad), whRepo, fwd, zap.NewNop())

	if err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`)); err != nil {
		t.Fatalf("dedup should be silent success, got %v", err)
	}
	if fwd.calls.Load() != 1 {
		t.Fatalf("want exactly 1 forward on dedup, got %d", fwd.calls.Load())
	}
	if whRepo.count() != 1 {
		t.Fatalf("want 1 row (dedup blocked second), got %d", whRepo.count())
	}
}

func TestWebhook_Ingest_BadSignature_RejectsForward(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.webhookSigOK = false
	ad.webhookEvent = &channel.WebhookEvent{EventID: "bad-sig", EventType: "charge.succeeded", PiID: "pi_1"}
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{}
	svc := NewWebhookService(newFakeRegistry(ad), whRepo, fwd, zap.NewNop())

	err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`))
	if !errors.Is(err, domain.ErrChannelSignatureFail) {
		t.Fatalf("want signature fail err got %v", err)
	}
	if fwd.calls.Load() != 0 {
		t.Fatal("must not forward when signature invalid")
	}
	// 但 webhook_raw 仍应落盘（做审计）
	if whRepo.count() != 1 {
		t.Fatalf("want 1 row for audit, got %d", whRepo.count())
	}
}

func TestWebhook_Ingest_ForwardErrorMarked(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.webhookEvent = &channel.WebhookEvent{EventID: "e", EventType: "charge.succeeded", PiID: "pi_1"}
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{err: errors.New("order-core 500")}
	svc := NewWebhookService(newFakeRegistry(ad), whRepo, fwd, zap.NewNop())

	err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`))
	if err == nil {
		t.Fatal("want forward error propagated")
	}
	rows, _ := whRepo.ListUnforwarded(context.Background(), 10)
	if len(rows) != 1 || rows[0].ForwardErr == "" {
		t.Fatalf("expected 1 unforwarded row with err, got %+v", rows)
	}
}

// ─── replay window ───────────────────────────────────────────────────────────

// Timestamp 偏现在 > replayWindow → 拒绝；不写 webhook_raw、不 forward。
func TestWebhook_Ingest_ReplayRejected(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.webhookEvent = &channel.WebhookEvent{
		EventID:     "old",
		EventType:   "charge.succeeded",
		PiID:        "pi_old",
		Timestamp:   time.Now().Add(-2 * time.Hour), // 远超 15min 默认 window
		SignatureOK: true,
	}
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{}
	svc := NewWebhookService(newFakeRegistry(ad), whRepo, fwd, zap.NewNop())

	err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`))
	if !errors.Is(err, domain.ErrWebhookReplay) {
		t.Fatalf("want replay error, got %v", err)
	}
	if fwd.calls.Load() != 0 {
		t.Fatal("must not forward old payload")
	}
	if whRepo.count() != 0 {
		t.Fatalf("must not insert old payload to webhook_raw, got %d", whRepo.count())
	}
}

// Timestamp 在 window 内 → 正常处理。
func TestWebhook_Ingest_ReplayWithinWindow(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.webhookEvent = &channel.WebhookEvent{
		EventID:     "fresh",
		EventType:   "charge.succeeded",
		PiID:        "pi_fresh",
		Timestamp:   time.Now().Add(-30 * time.Second),
		SignatureOK: true,
	}
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{}
	svc := NewWebhookService(newFakeRegistry(ad), whRepo, fwd, zap.NewNop())

	if err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`)); err != nil {
		t.Fatalf("fresh payload should succeed, got %v", err)
	}
	if fwd.calls.Load() != 1 {
		t.Fatal("expected 1 forward")
	}
}

// 显式禁用 replay 检查（ReplayWindow < 0），老 payload 也通过。
func TestWebhook_Ingest_ReplayDisabledByOption(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.webhookEvent = &channel.WebhookEvent{
		EventID:     "old-but-allowed",
		EventType:   "charge.succeeded",
		PiID:        "pi_x",
		Timestamp:   time.Now().Add(-24 * time.Hour),
		SignatureOK: true,
	}
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{}
	svc := NewWebhookServiceWithOptions(
		newFakeRegistry(ad), whRepo, fwd, zap.NewNop(),
		WebhookOptions{ReplayWindow: -1},
	)
	if err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`)); err != nil {
		t.Fatalf("expected success when replay check disabled, got %v", err)
	}
}

// Timestamp 零值（老 adapter 没填）→ 跳过检查（向后兼容），仍正常处理。
func TestWebhook_Ingest_ReplayZeroTimestampPasses(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.webhookEvent = &channel.WebhookEvent{
		EventID:     "no-ts",
		EventType:   "charge.succeeded",
		PiID:        "pi_z",
		SignatureOK: true,
	}
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{}
	svc := NewWebhookService(newFakeRegistry(ad), whRepo, fwd, zap.NewNop())
	if err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`)); err != nil {
		t.Fatalf("zero timestamp should pass, got %v", err)
	}
}

// ─── per-adapter rate limit ──────────────────────────────────────────────────

// 限流命中 → 不调 Parse，不写 webhook_raw，返回 ErrWebhookRateLimited。
func TestWebhook_Ingest_RateLimited(t *testing.T) {
	ad := newFakeAdapter("gcash")
	ad.webhookEvent = &channel.WebhookEvent{
		EventID: "x", EventType: "charge.succeeded", PiID: "pi_x",
		Timestamp: time.Now(), SignatureOK: true,
	}
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{}
	// burst=1 → 第二次应被限。RPS=0.0001 让 refill 速度极慢，测试期间稳定。
	svc := NewWebhookServiceWithOptions(
		newFakeRegistry(ad), whRepo, fwd, zap.NewNop(),
		WebhookOptions{AdapterRPS: 0.0001, AdapterBurst: 1},
	)

	if err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`)); err != nil {
		t.Fatalf("first should pass, got %v", err)
	}
	err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`))
	if !errors.Is(err, domain.ErrWebhookRateLimited) {
		t.Fatalf("second should be rate limited, got %v", err)
	}
	// rate-limited 路径没有 forward；第一个请求 forward 了一次。
	if fwd.calls.Load() != 1 {
		t.Fatalf("expected exactly 1 forward, got %d", fwd.calls.Load())
	}
}

// 不同 adapter 各自独立桶。
func TestWebhook_Ingest_RateLimitPerAdapter(t *testing.T) {
	gcash := newFakeAdapter("gcash")
	gcash.webhookEvent = &channel.WebhookEvent{
		EventID: "g1", EventType: "charge.succeeded", PiID: "pi_g",
		Timestamp: time.Now(), SignatureOK: true,
	}
	maya := newFakeAdapter("maya")
	maya.webhookEvent = &channel.WebhookEvent{
		EventID: "m1", EventType: "charge.succeeded", PiID: "pi_m",
		Timestamp: time.Now(), SignatureOK: true,
	}
	reg := newFakeRegistry(gcash, maya)
	whRepo := newMemWebhookRepo()
	fwd := &captureForwarder{}
	svc := NewWebhookServiceWithOptions(
		reg, whRepo, fwd, zap.NewNop(),
		WebhookOptions{AdapterRPS: 0.0001, AdapterBurst: 1},
	)

	// 各 adapter 的 burst=1 都该过第一次
	if err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`)); err != nil {
		t.Fatalf("gcash first should pass, got %v", err)
	}
	if err := svc.Ingest(context.Background(), "maya", nil, []byte(`{}`)); err != nil {
		t.Fatalf("maya first should pass independently of gcash, got %v", err)
	}
	// 第二次同 adapter 应被限
	if err := svc.Ingest(context.Background(), "gcash", nil, []byte(`{}`)); !errors.Is(err, domain.ErrWebhookRateLimited) {
		t.Fatalf("gcash second should be rate limited, got %v", err)
	}
}
