// handler_test.go: card-center HTTPS REST 单测覆盖。
//
// 测试矩阵：
//
//	auth                 → 401 if no jwt / invalid jwt
//	cross-user           → 403 if body.user_id != ctx.user_id
//	luhn                 → 400 if PAN checksum invalid
//	method not allowed   → 405 PUT/PATCH /v1/cards
//	tokenize happy path  → 200 + masked_pan in resp
//	list happy path      → 200 + only masked-only fields
//	delete happy path    → 200 + status=deleted
//	delete other user    → 404 (因为 SoftDeleteByID where user_id 不命中)
//	security headers     → HSTS / X-Frame-Options / Cache-Control 都在
//
// 测试用 fakeVerifier + fakeCardOps，不依赖 vault / repo / KMS。
package httpsauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/card-center/internal/service"
)

// ─── fakes ──────────────────────────────────────────────────────────────────

type fakeVerifier struct {
	jwt    string
	userID int64
	err    error
}

func (f *fakeVerifier) Verify(_ context.Context, jwt string) (int64, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	if jwt != f.jwt {
		return 0, false, ErrInvalidJWT
	}
	return f.userID, true, nil
}

type fakeCardOps struct {
	mu sync.Mutex
	// 简单 map：userID → []card
	cards map[int64][]*service.CardDisplay
	// 可注入错误
	tokenizeErr error
	listErr     error
	deleteErr   error
	// 调用计数
	tokenizeCalls, listCalls, deleteCalls int
}

func newFakeCardOps() *fakeCardOps {
	return &fakeCardOps{cards: make(map[int64][]*service.CardDisplay)}
}

func (f *fakeCardOps) Tokenize(_ context.Context, in *service.TokenizeInput) (*service.TokenizeOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenizeCalls++
	if f.tokenizeErr != nil {
		return nil, f.tokenizeErr
	}
	if in.PAN == "" || in.UserID == 0 {
		return nil, errors.New("invalid input")
	}
	masked := "411111******" + in.PAN[len(in.PAN)-4:]
	id := int64(len(f.cards[in.UserID]) + 1)
	f.cards[in.UserID] = append(f.cards[in.UserID], &service.CardDisplay{
		UserCardID: id, MaskedPAN: masked, Network: "visa",
		ExpMonth: in.ExpMonth, ExpYear: in.ExpYear,
		HolderName: in.HolderName, Status: "active",
	})
	return &service.TokenizeOutput{
		StoredToken: "tok_card_fake_" + in.PAN[len(in.PAN)-4:],
		MaskedPAN:   masked,
		Network:     "visa",
	}, nil
}

func (f *fakeCardOps) ListUserCards(_ context.Context, userID int64, _, _, _ string) ([]*service.CardDisplay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*service.CardDisplay, 0, len(f.cards[userID]))
	for _, c := range f.cards[userID] {
		if c.Status == "active" {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeCardOps) DeleteCardByID(_ context.Context, userID, id int64, _, _, _, _ string) (*service.CardDisplay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	for _, c := range f.cards[userID] {
		if c.UserCardID == id {
			c.Status = "deleted"
			return c, nil
		}
	}
	return nil, errors.New("not found")
}

// ─── helpers ───────────────────────────────────────────────────────────────

func newTestServer(t *testing.T, vfy Verifier, ops CardOps) http.Handler {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	return NewRESTServer(ops, vfy, logger).Handler()
}

func doReq(t *testing.T, h http.Handler, method, path, jwt string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, rdr)
	if jwt != "" {
		r.Header.Set("Authorization", "Bearer "+jwt)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// ─── auth tests ────────────────────────────────────────────────────────────

func TestAuth_NoJWT_401(t *testing.T) {
	h := newTestServer(t, &fakeVerifier{jwt: "good", userID: 100}, newFakeCardOps())
	w := doReq(t, h, http.MethodGet, "/v1/cards", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAuth_InvalidJWT_401(t *testing.T) {
	h := newTestServer(t, &fakeVerifier{jwt: "good", userID: 100}, newFakeCardOps())
	w := doReq(t, h, http.MethodGet, "/v1/cards", "wrong", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuth_HealthzNoAuth_200(t *testing.T) {
	h := newTestServer(t, &fakeVerifier{jwt: "good", userID: 100}, newFakeCardOps())
	w := doReq(t, h, http.MethodGet, "/healthz", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

// ─── cross-user rejection (strict mode) ─────────────────────────────────────

func TestTokenize_BodyHasUserID_403(t *testing.T) {
	ops := newFakeCardOps()
	h := newTestServer(t, &fakeVerifier{jwt: "good", userID: 100}, ops)
	// 严格模式：body 里有 user_id 一律拒，不论是否跟 ctx 匹配
	for _, claimed := range []int64{999, 100} {
		body := map[string]any{
			"pan": "4111111111111111", "exp_month": 12, "exp_year": 2030,
			"cvv": "123", "user_id": claimed,
		}
		w := doReq(t, h, http.MethodPost, "/v1/cards", "good", body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("claimed=%d: want 403, got %d body=%s", claimed, w.Code, w.Body.String())
		}
	}
	if ops.tokenizeCalls != 0 {
		t.Fatalf("service Tokenize 不应被调用")
	}
}

// ─── Luhn validation ───────────────────────────────────────────────────────

func TestTokenize_BadLuhn_400(t *testing.T) {
	ops := newFakeCardOps()
	h := newTestServer(t, &fakeVerifier{jwt: "good", userID: 100}, ops)
	body := map[string]any{
		// 最后一位改 0，校验和不对
		"pan": "4111111111111110", "exp_month": 12, "exp_year": 2030, "cvv": "123",
	}
	w := doReq(t, h, http.MethodPost, "/v1/cards", "good", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if ops.tokenizeCalls != 0 {
		t.Fatalf("Tokenize 不应被调用（Luhn 失败应在 handler 层短路）")
	}
}

// ─── happy path: tokenize → list → delete ──────────────────────────────────

func TestFlow_TokenizeListDelete(t *testing.T) {
	ops := newFakeCardOps()
	h := newTestServer(t, &fakeVerifier{jwt: "good", userID: 100}, ops)

	// 1. tokenize
	body := map[string]any{
		"pan": "4111111111111111", "exp_month": 12, "exp_year": 2030,
		"cvv": "123", "holder_name": "ALICE",
	}
	w := doReq(t, h, http.MethodPost, "/v1/cards", "good", body)
	if w.Code != http.StatusOK {
		t.Fatalf("tokenize: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var tokResp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &tokResp)
	if !strings.HasPrefix(tokResp["masked_pan"].(string), "411111") {
		t.Fatalf("masked_pan: %v", tokResp["masked_pan"])
	}

	// 2. list 应返 1 张
	w = doReq(t, h, http.MethodGet, "/v1/cards", "good", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", w.Code)
	}
	var listResp struct {
		Cards []map[string]any `json:"cards"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Cards) != 1 {
		t.Fatalf("len cards: %d", len(listResp.Cards))
	}
	cardID, _ := listResp.Cards[0]["id"].(float64)

	// list 不应包含 stored_token / kms_kid
	if _, has := listResp.Cards[0]["stored_token"]; has {
		t.Fatalf("list 不应吐 stored_token")
	}

	// 3. delete
	w = doReq(t, h, http.MethodDelete, "/v1/cards/"+itoa(int64(cardID)), "good", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d body=%s", w.Code, w.Body.String())
	}

	// 4. list 应返 0
	w = doReq(t, h, http.MethodGet, "/v1/cards", "good", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Cards) != 0 {
		t.Fatalf("after delete len cards: %d", len(listResp.Cards))
	}
}

// ─── delete other user's card → 404 ────────────────────────────────────────

func TestDelete_OtherUser_404(t *testing.T) {
	ops := newFakeCardOps()
	// user 200 先建一张卡
	ops.cards[200] = []*service.CardDisplay{
		{UserCardID: 7, MaskedPAN: "411111******1111", Status: "active"},
	}
	// user 100 登录
	h := newTestServer(t, &fakeVerifier{jwt: "good", userID: 100}, ops)

	w := doReq(t, h, http.MethodDelete, "/v1/cards/7", "good", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", w.Code, w.Body.String())
	}
	// user 200 的卡仍在
	if ops.cards[200][0].Status != "active" {
		t.Fatalf("user 200 的卡状态被改了")
	}
}

// ─── method not allowed ────────────────────────────────────────────────────

func TestMethod_PUT_405(t *testing.T) {
	h := newTestServer(t, &fakeVerifier{jwt: "good", userID: 100}, newFakeCardOps())
	w := doReq(t, h, http.MethodPut, "/v1/cards", "good", nil)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

// ─── security headers ──────────────────────────────────────────────────────

func TestSecurityHeaders_AllResponses(t *testing.T) {
	h := newTestServer(t, &fakeVerifier{jwt: "good", userID: 100}, newFakeCardOps())
	w := doReq(t, h, http.MethodGet, "/healthz", "", nil)
	checks := map[string]string{
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "no-referrer",
		"Cache-Control":             "no-store",
	}
	for k, want := range checks {
		if got := w.Header().Get(k); got != want {
			t.Errorf("header %s: want %q, got %q", k, want, got)
		}
	}
}

// ─── rate limit ────────────────────────────────────────────────────────────

func TestRateLimit_Tokenize_429AfterBurst(t *testing.T) {
	ops := newFakeCardOps()
	rl := NewMemoryBucket(2, time.Hour) // burst=2，1h 才补，本测试观察不到补
	logger, _ := zap.NewDevelopment()
	rest := NewRESTServer(ops, &fakeVerifier{jwt: "good", userID: 100}, logger).WithRateLimiter(rl)
	h := rest.Handler()

	body := map[string]any{
		"pan": "4111111111111111", "exp_month": 12, "exp_year": 2030, "cvv": "123",
	}
	// 头 2 次 200
	for i := 0; i < 2; i++ {
		w := doReq(t, h, http.MethodPost, "/v1/cards", "good", body)
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d: want 200, got %d body=%s", i, w.Code, w.Body.String())
		}
	}
	// 第 3 次 429
	w := doReq(t, h, http.MethodPost, "/v1/cards", "good", body)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd attempt: want 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatalf("缺 Retry-After header")
	}
	// 限流不影响 GET（list）—— 不同路径
	w = doReq(t, h, http.MethodGet, "/v1/cards", "good", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 不应被限流: got %d", w.Code)
	}
}

func TestRateLimit_PerUser(t *testing.T) {
	rl := NewMemoryBucket(2, time.Hour)
	// user 100 用完
	for i := 0; i < 2; i++ {
		ok, _, _ := rl.Allow(100)
		if !ok {
			t.Fatalf("uid 100 attempt %d should pass", i)
		}
	}
	if ok, _, _ := rl.Allow(100); ok {
		t.Fatalf("uid 100 第 3 次应该被限")
	}
	// user 200 桶独立
	if ok, _, _ := rl.Allow(200); !ok {
		t.Fatalf("uid 200 第 1 次应该通过（桶独立）")
	}
}

// ─── Luhn function 单元 ────────────────────────────────────────────────────

func TestLuhn(t *testing.T) {
	cases := map[string]bool{
		"4111111111111111":     true,
		"4111 1111 1111 1111":  true,
		"4111-1111-1111-1111":  true,
		"4111111111111110":     false, // 改了校验位
		"":                     false,
		"abc":                  false,
		"123":                  false, // 太短
		"5555555555554444":     true,  // master test
		"6011111111111117":     true,  // discover test
		"371449635398431":      true,  // amex test
	}
	for in, want := range cases {
		if got := luhnValid(in); got != want {
			t.Errorf("luhn(%q) = %v, want %v", in, got, want)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
