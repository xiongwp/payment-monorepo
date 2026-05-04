package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xiongwp/risk-manage/internal/auth"
)

func TestAdminAuthRoles_AcceptsValidToken(t *testing.T) {
	roles := map[string]string{"goodtok": "danger"}
	mw := AdminAuthRoles(roles)
	called := false
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p == nil {
			t.Fatal("expected Principal in ctx")
		}
		if p.AdminRole != "danger" {
			t.Fatalf("expected role=danger; got %q", p.AdminRole)
		}
		if p.KeyID == "" {
			t.Fatal("expected KeyID populated")
		}
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer goodtok")
	h.ServeHTTP(rr, req)
	if !called {
		t.Fatalf("handler not called; status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminAuthRoles_RejectsBadToken(t *testing.T) {
	roles := map[string]string{"goodtok": "read"}
	mw := AdminAuthRoles(roles)
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("should not be called")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer badtok")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401; got %d", rr.Code)
	}
}

func TestAdminAuthRoles_EmptyMapAllowAll(t *testing.T) {
	mw := AdminAuthRoles(nil)
	called := false
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatal("dev mode should allow all")
	}
}

func TestRequireRole_DenyReadAccessingWrite(t *testing.T) {
	// read token 访问 write 路径被拒
	mw := RequireRole("write")
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("should not pass")
	}))
	ctx := auth.WithPrincipal(context.Background(),
		&auth.Principal{KeyID: "tok_x", AdminRole: "read"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403; got %d", rr.Code)
	}
}

func TestRequireRole_AllowDangerHasWrite(t *testing.T) {
	mw := RequireRole("write")
	called := false
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	ctx := auth.WithPrincipal(context.Background(),
		&auth.Principal{KeyID: "tok_y", AdminRole: "danger"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	if !called || rr.Code != 200 {
		t.Fatalf("danger role should pass write check; called=%v code=%d", called, rr.Code)
	}
}

func TestRequireRole_NoPrincipalBypasses(t *testing.T) {
	// 无 Principal (dev 模式) → 放行（不强制 RBAC）
	mw := RequireRole("danger")
	called := false
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatal("dev mode (no Principal) should allow")
	}
}

func TestPrincipalHasRole_Hierarchy(t *testing.T) {
	read := &auth.Principal{AdminRole: "read"}
	write := &auth.Principal{AdminRole: "write"}
	danger := &auth.Principal{AdminRole: "danger"}

	if !read.HasRole("read") {
		t.Error("read should have read")
	}
	if read.HasRole("write") {
		t.Error("read should NOT have write")
	}
	if !write.HasRole("read") {
		t.Error("write should imply read")
	}
	if !danger.HasRole("write") {
		t.Error("danger should imply write")
	}
}
