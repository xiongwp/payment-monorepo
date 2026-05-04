package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/gorilla/mux"
	"google.golang.org/grpc"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

// fakeMerchant 只覆盖测试用到的两个方法；其它方法走 embedded nil interface
// （任何意外调用都会 panic 暴露误用）。
type fakeMerchant struct {
	usermerchantv1.MerchantServiceClient
	rotateCalls atomic.Int32
	kycCalls    atomic.Int32
}

func (f *fakeMerchant) RotateApiKey(_ context.Context, req *usermerchantv1.RotateApiKeyRequest, _ ...grpc.CallOption) (*usermerchantv1.RotateApiKeyResponse, error) {
	n := f.rotateCalls.Add(1)
	return &usermerchantv1.RotateApiKeyResponse{Plaintext: "key_" + strconv.Itoa(int(n))}, nil
}

func (f *fakeMerchant) Approve(_ context.Context, req *usermerchantv1.KycTransitionRequest, _ ...grpc.CallOption) (*usermerchantv1.KycTransitionResponse, error) {
	f.kycCalls.Add(1)
	return &usermerchantv1.KycTransitionResponse{Merchant: &usermerchantv1.Merchant{Id: req.Id, KycStatus: usermerchantv1.KycStatus_KYC_STATUS_APPROVED}}, nil
}

func newMerchantRouter(_ *testing.T, fm *fakeMerchant) http.Handler {
	h := NewMerchantHandler(clients.Deps{Merchant: fm})
	r := mux.NewRouter()
	api := r.PathPrefix("/api").Subrouter()
	api.HandleFunc("/merchants/{id}/rotate-key", h.RotateAPIKey).Methods("POST")
	api.HandleFunc("/merchants/{id}/kyc/{action}", h.KYCTransition).Methods("POST")
	return r
}

func postJSON(t *testing.T, h http.Handler, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestRotateAPIKey_IdempotencyKey_DedupesDoubleSubmit(t *testing.T) {
	fm := &fakeMerchant{}
	h := newMerchantRouter(t, fm)

	body := map[string]any{"kind": "live", "idempotency_key": "rk-abc-123"}
	code1, out1 := postJSON(t, h, "/api/merchants/m_1/rotate-key", body, nil)
	if code1 != http.StatusOK {
		t.Fatalf("first call status=%d", code1)
	}
	code2, out2 := postJSON(t, h, "/api/merchants/m_1/rotate-key", body, nil)
	if code2 != http.StatusOK {
		t.Fatalf("second call status=%d", code2)
	}
	// **关键**：upstream gRPC 只能被调一次（否则等于真签发了两把 key，
	// 第一把直接「丢失」给 ops，但已经在 user-merchant-core 落库）。
	if got := fm.rotateCalls.Load(); got != 1 {
		t.Fatalf("RotateApiKey upstream called %d times, want 1 (regression: 双击会签发两把 key)", got)
	}
	if out1["plaintext"] != out2["plaintext"] {
		t.Fatalf("replay should return identical plaintext, got %v vs %v", out1["plaintext"], out2["plaintext"])
	}
}

func TestRotateAPIKey_IdempotencyHeader_AlsoWorks(t *testing.T) {
	fm := &fakeMerchant{}
	h := newMerchantRouter(t, fm)

	body := map[string]any{"kind": "test"}
	headers := map[string]string{"Idempotency-Key": "rk-hdr-1"}
	_, out1 := postJSON(t, h, "/api/merchants/m_2/rotate-key", body, headers)
	_, out2 := postJSON(t, h, "/api/merchants/m_2/rotate-key", body, headers)
	if got := fm.rotateCalls.Load(); got != 1 {
		t.Fatalf("Idempotency-Key header dedup failed; upstream called %d times", got)
	}
	if out1["plaintext"] != out2["plaintext"] {
		t.Fatalf("header dedup returned different plaintext")
	}
}

func TestRotateAPIKey_DifferentKind_NotDeduped(t *testing.T) {
	// 同一 idempotency_key 但 kind 不同（live vs test）—— 必须分别签发，不能 cache 跨 kind。
	fm := &fakeMerchant{}
	h := newMerchantRouter(t, fm)
	headers := map[string]string{"Idempotency-Key": "rk-mix"}
	postJSON(t, h, "/api/merchants/m_3/rotate-key", map[string]any{"kind": "live"}, headers)
	postJSON(t, h, "/api/merchants/m_3/rotate-key", map[string]any{"kind": "test"}, headers)
	if got := fm.rotateCalls.Load(); got != 2 {
		t.Fatalf("live + test should NOT share cache; upstream called %d times, want 2", got)
	}
}

func TestKYCTransition_IdempotencyKey_DedupesDoubleSubmit(t *testing.T) {
	fm := &fakeMerchant{}
	h := newMerchantRouter(t, fm)
	body := map[string]any{"actor": "alice", "idempotency_key": "ky-1"}
	postJSON(t, h, "/api/merchants/m_4/kyc/approve", body, nil)
	postJSON(t, h, "/api/merchants/m_4/kyc/approve", body, nil)
	if got := fm.kycCalls.Load(); got != 1 {
		t.Fatalf("KYC approve dedup failed; upstream called %d times", got)
	}
}
