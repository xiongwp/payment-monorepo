package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gorilla/mux"
	"google.golang.org/grpc"

	usermerchantv1 "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

// fakeAudit 只实现拦截器最终用到的 ListAuditLogs；其余方法 panic 暴露误用。
type fakeAudit struct {
	umauditservice.Client
	gotReq *usermerchantv1.ListAuditLogsRequest
	resp   *usermerchantv1.ListAuditLogsResponse
}

func (f *fakeAudit) List(_ context.Context, req *usermerchantv1.ListAuditLogsRequest, _ ...grpc.CallOption) (*usermerchantv1.ListAuditLogsResponse, error) {
	f.gotReq = req
	return f.resp, nil
}

func TestUserMerchantAudit_List_PassesFiltersToGRPC(t *testing.T) {
	fa := &fakeAudit{
		resp: &usermerchantv1.ListAuditLogsResponse{
			Entries: []*usermerchantv1.AuditLogEntry{
				{Id: 1, Actor: "alice", Method: "/m/Update", StatusCode: "OK"},
			},
		},
	}
	h := NewUserMerchantAuditHandler(clients.Deps{UserMerchantAudit: fa})

	r := mux.NewRouter()
	r.HandleFunc("/api/user-merchant/audits", h.List).Methods("GET")

	u, _ := url.Parse("/api/user-merchant/audits")
	q := u.Query()
	q.Set("actor", "alice")
	q.Set("target", "mch_42")
	q.Set("limit", "10")
	q.Set("offset", "20")
	u.RawQuery = q.Encode()

	req := httptest.NewRequest(http.MethodGet, u.String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if fa.gotReq.GetActor() != "alice" || fa.gotReq.GetTarget() != "mch_42" {
		t.Fatalf("filters not forwarded: %+v", fa.gotReq)
	}
	if fa.gotReq.GetLimit() != 10 || fa.gotReq.GetOffset() != 20 {
		t.Fatalf("pagination not forwarded: limit=%d offset=%d", fa.gotReq.GetLimit(), fa.gotReq.GetOffset())
	}

	// writeJSON 包一层 {code,message,data:{...}}；entries 在 data 下。
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v body=%s", err, rr.Body.String())
	}
	data, _ := out["data"].(map[string]any)
	entries, _ := data["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entry count: got %d, body=%s", len(entries), rr.Body.String())
	}
}

func TestUserMerchantAudit_List_DefaultLimit(t *testing.T) {
	fa := &fakeAudit{resp: &usermerchantv1.ListAuditLogsResponse{}}
	h := NewUserMerchantAuditHandler(clients.Deps{UserMerchantAudit: fa})

	r := mux.NewRouter()
	r.HandleFunc("/api/user-merchant/audits", h.List).Methods("GET")

	req := httptest.NewRequest(http.MethodGet, "/api/user-merchant/audits", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if fa.gotReq.GetLimit() != 50 {
		t.Fatalf("default limit should be 50, got %d", fa.gotReq.GetLimit())
	}
}
