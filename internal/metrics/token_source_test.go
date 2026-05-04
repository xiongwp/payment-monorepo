package metrics

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStaticTokenSource_TrimsAndDedups(t *testing.T) {
	s := NewStaticTokenSource([]string{"  abc  ", "abc", "", "def"})
	tk := s.Tokens()
	if _, ok := tk["abc"]; !ok {
		t.Fatal("abc should be present (trimmed)")
	}
	if _, ok := tk["def"]; !ok {
		t.Fatal("def should be present")
	}
	if len(tk) != 2 {
		t.Fatalf("expected 2 unique tokens, got %d: %v", len(tk), tk)
	}
}

func TestFileTokenSource_LoadAndHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(path, []byte("# comment\nalpha\nbeta\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := NewFileTokenSource(path, 50*time.Millisecond, nil)
	defer src.Stop()

	if tk := src.Tokens(); len(tk) != 2 {
		t.Fatalf("initial load: expected 2 tokens, got %d: %v", len(tk), tk)
	}

	// 轮换：alpha 撤销，加 gamma
	if err := os.WriteFile(path, []byte("beta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 等下次 reload tick
	time.Sleep(150 * time.Millisecond)
	tk := src.Tokens()
	if _, has := tk["alpha"]; has {
		t.Fatal("alpha should have been revoked after reload")
	}
	if _, has := tk["gamma"]; !has {
		t.Fatal("gamma should be present after reload")
	}
}

func TestAdminAuthFromSource_RejectMissingToken(t *testing.T) {
	src := NewStaticTokenSource([]string{"valid"})
	mw := AdminAuthFromSource(src)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing Authorization should be 401, got %d", rec.Code)
	}
}

func TestAdminAuthFromSource_AcceptValidToken(t *testing.T) {
	src := NewStaticTokenSource([]string{"valid"})
	mw := AdminAuthFromSource(src)
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/admin/x", nil)
	req.Header.Set("Authorization", "Bearer valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("valid token should pass through, got status=%d called=%t", rec.Code, called)
	}
}

func TestAdminAuthFromSource_RejectInvalidToken(t *testing.T) {
	src := NewStaticTokenSource([]string{"valid"})
	mw := AdminAuthFromSource(src)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/admin/x", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token should be 401, got %d", rec.Code)
	}
}

func TestAdminAuthFromSource_EmptyTokensFallthrough(t *testing.T) {
	// dev mode: 空 source → 等价 NoOp
	src := NewStaticTokenSource(nil)
	mw := AdminAuthFromSource(src)
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/x", nil))
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("empty source must fall through (dev mode), got status=%d called=%t", rec.Code, called)
	}
}
