// biz-admin-web — 7 服务的统一 admin 控制台（CORS reverse proxy + 单页 HTML）。
//
// 浏览器只跟 :18080 通信，避免 CORS 在前端折腾。
// 后端按路径前缀 proxy 到对应业务 service:
//   /api/billing/*      → billing-system:8080
//   /api/gateway/*      → payment-gateway:8080
//   /api/dispute/*      → dispute-service:8080
//   /api/webhook/*      → merchant-webhook:8080
//   /api/refund/*       → refund-engine:8080
//   /api/kyc/*          → kyc-service:8080
//   /api/audit/*        → audit-log:8080
//   /                   → embedded admin.html (Tailwind + Alpine.js CDN)

package main

import (
	"context"
	_ "embed"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

//go:embed ../../static/admin.html
var adminHTML []byte

type backend struct {
	prefix string // /api/billing/
	target string // http://billing-system:8080
}

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := envOr("BIZ_ADMIN_HTTP_PORT", "8080")

	backends := []backend{
		{"/api/billing/", envOr("BILLING_URL", "http://billing-system:8080")},
		{"/api/gateway/", envOr("GATEWAY_URL", "http://payment-gateway:8080")},
		{"/api/dispute/", envOr("DISPUTE_URL", "http://dispute-service:8080")},
		{"/api/webhook/", envOr("WEBHOOK_URL", "http://merchant-webhook:8080")},
		{"/api/refund/", envOr("REFUND_URL", "http://refund-engine:8080")},
		{"/api/kyc/", envOr("KYC_URL", "http://kyc-service:8080")},
		{"/api/audit/", envOr("AUDIT_URL", "http://audit-log:8080")},
		{"/api/recon/", envOr("RECON_URL", "http://reconplatform-admin:8080")},
	}

	mux := http.NewServeMux()
	for _, b := range backends {
		b := b
		mux.HandleFunc(b.prefix, func(w http.ResponseWriter, r *http.Request) {
			proxy(w, r, b.prefix, b.target, logger)
		})
	}
	mux.HandleFunc("/health/all", healthAll(backends, logger))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/admin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(adminHTML)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger.Info("biz-admin-web listening", zap.String("addr", srv.Addr))
	go srv.ListenAndServe()
	<-ctx.Done()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

// proxy 简单反代 — 把 /api/<svc>/foo 改写成 target/<foo>。
func proxy(w http.ResponseWriter, r *http.Request, prefix, target string, log *zap.Logger) {
	upstreamPath := "/" + strings.TrimPrefix(r.URL.Path, prefix)
	u, _ := url.Parse(target + upstreamPath)
	if r.URL.RawQuery != "" {
		u.RawQuery = r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("proxy failed",
			zap.String("upstream", u.String()), zap.Error(err))
		http.Error(w, "upstream unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// healthAll 一次性 probe 所有后端 — 给 admin 顶部状态条用。
func healthAll(backends []backend, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client := &http.Client{Timeout: 2 * time.Second}
		out := map[string]string{}
		for _, b := range backends {
			start := time.Now()
			resp, err := client.Get(b.target + "/healthz")
			elapsed := time.Since(start)
			name := strings.TrimSuffix(strings.TrimPrefix(b.prefix, "/api/"), "/")
			if err != nil {
				out[name] = "down (" + err.Error() + ")"
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == 200 {
				out[name] = "ok " + elapsed.String()
			} else {
				out[name] = "unhealthy (status=" + resp.Status + ")"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, out)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	body := []byte("{")
	if m, ok := v.(map[string]string); ok {
		first := true
		for k, val := range m {
			if !first {
				body = append(body, ',')
			}
			first = false
			body = append(body, '"')
			body = append(body, []byte(k)...)
			body = append(body, '"', ':', '"')
			body = append(body, []byte(val)...)
			body = append(body, '"')
		}
	}
	body = append(body, '}')
	w.Write(body)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
