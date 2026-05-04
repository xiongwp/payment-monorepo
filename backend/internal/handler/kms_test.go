package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	kmsth "github.com/xiongwp/kms-manage/testhelper"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

func newKMSRouter(t *testing.T) (*mux.Router, kmsv1.KMSServiceClient) {
	t.Helper()
	srv := kmsth.Start(t, kmsth.DefaultConfig())

	deps := clients.Deps{KMS: srv.Client}
	h := NewKMSHandler(deps)

	r := mux.NewRouter()
	api := r.PathPrefix("/api").Subrouter()
	api.Use(CORSMiddleware, AuthMiddleware(""))
	api.HandleFunc("/kms/keys", h.List).Methods("GET")
	api.HandleFunc("/kms/encrypt", h.Encrypt).Methods("POST")
	api.HandleFunc("/kms/decrypt", h.Decrypt).Methods("POST")
	return r, srv.Client
}

func doReq(t *testing.T, r http.Handler, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var reader *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewBuffer(b)
	} else {
		reader = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]interface{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

func TestKMSHandler_ListKeys(t *testing.T) {
	r, _ := newKMSRouter(t)
	code, body := doReq(t, r, "GET", "/api/kms/keys", nil)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	data := body["data"].(map[string]interface{})
	if data["active_key_id"] != "main" {
		t.Fatalf("active=%v", data["active_key_id"])
	}
	items := data["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("len=%d", len(items))
	}
}

func TestKMSHandler_EncryptDecrypt_RoundTrip(t *testing.T) {
	r, _ := newKMSRouter(t)

	code, enc := doReq(t, r, "POST", "/api/kms/encrypt", map[string]string{
		"plaintext": "some-secret",
		"context":   "svc:admin:test",
	})
	if code != 200 {
		t.Fatalf("encrypt code=%d body=%v", code, enc)
	}
	ct := enc["data"].(map[string]interface{})["ciphertext"].(string)
	if !strings.HasPrefix(ct, "kms:v1:main:") {
		t.Fatalf("ct=%s", ct)
	}

	code, dec := doReq(t, r, "POST", "/api/kms/decrypt", map[string]string{
		"ciphertext": ct,
		"context":    "svc:admin:test",
	})
	if code != 200 {
		t.Fatalf("decrypt code=%d body=%v", code, dec)
	}
	if dec["data"].(map[string]interface{})["plaintext"] != "some-secret" {
		t.Fatalf("got %v", dec)
	}
}

func TestKMSHandler_EncryptRequiresPlaintext(t *testing.T) {
	r, _ := newKMSRouter(t)
	code, body := doReq(t, r, "POST", "/api/kms/encrypt", map[string]string{"plaintext": ""})
	if code != 400 {
		t.Fatalf("code=%d body=%v", code, body)
	}
}

func TestAuthMiddleware_RejectsWrongToken(t *testing.T) {
	srv := kmsth.Start(t, kmsth.DefaultConfig())
	h := NewKMSHandler(clients.Deps{KMS: srv.Client})
	r := mux.NewRouter()
	api := r.PathPrefix("/api").Subrouter()
	api.Use(AuthMiddleware("right-token"))
	api.HandleFunc("/kms/keys", h.List).Methods("GET")

	// 无 token
	req := httptest.NewRequest("GET", "/api/kms/keys", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("no token code=%d", w.Code)
	}

	// 错 token
	req = httptest.NewRequest("GET", "/api/kms/keys", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("wrong token code=%d", w.Code)
	}

	// 对 token
	req = httptest.NewRequest("GET", "/api/kms/keys", nil)
	req.Header.Set("Authorization", "Bearer right-token")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("right token code=%d", w.Code)
	}
}

// silence unused-import on context
var _ = context.Background
