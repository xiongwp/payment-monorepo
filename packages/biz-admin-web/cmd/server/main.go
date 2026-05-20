// biz-admin-web — 7 服务的统一 admin 控制台 (CORS reverse proxy + 单页 HTML).
//
// uber/fx 装配, 跟 order-core / accounting-system 同款风格.
//
// 浏览器只跟 :18080 通信, 避免 CORS 在前端折腾.
// 后端按路径前缀 proxy 到对应业务 service.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
)

//go:embed ../../static/admin.html
var adminHTML []byte

//go:embed ../../static/p0-services.html
var p0HTML []byte

//go:embed ../../static/approval.html
var approvalHTML []byte

type backend struct {
	prefix string // /api/billing/
	target string // http://billing-system:8080
}

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newBackends,
			newHTTPServer,
		),
		fx.Invoke(startHTTPServer),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
	).Run()
}

func newLogger(lc fx.Lifecycle) (*zap.Logger, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { _ = logger.Sync(); return nil }})
	return logger, nil
}

func newBackends() []backend {
	return []backend{
		{"/api/billing/", envOr("BILLING_URL", "http://billing-system:8080")},
		{"/api/gateway/", envOr("GATEWAY_URL", "http://payment-gateway:8080")},
		{"/api/dispute/", envOr("DISPUTE_URL", "http://dispute-service:8080")},
		{"/api/webhook/", envOr("WEBHOOK_URL", "http://merchant-webhook:8080")},
		{"/api/refund/", envOr("REFUND_URL", "http://refund-engine:8080")},
		{"/api/kyc/", envOr("KYC_URL", "http://kyc-service:8080")},
		{"/api/audit/", envOr("AUDIT_URL", "http://audit-log:8080")},
		{"/api/recon/", envOr("RECON_URL", "http://reconplatform-admin:8080")},
		{"/api/aml/", envOr("AML_URL", "http://aml-screening:8088")},
		{"/api/vault/", envOr("VAULT_URL", "http://tokenization-vault:8089")},
		{"/api/tax/", envOr("TAX_URL", "http://tax-reporting:8090")},
		{"/api/dr/", envOr("DR_URL", "http://data-rights:8091")},
		{"/api/approval/", envOr("APPROVAL_URL", "http://approval-service:8092")},
	}
}

func newHTTPServer(backends []backend, log *zap.Logger) *http.Server {
	mux := http.NewServeMux()
	for _, b := range backends {
		b := b
		mux.HandleFunc(b.prefix, func(w http.ResponseWriter, r *http.Request) {
			proxy(w, r, b.prefix, b.target, log)
		})
	}
	mux.HandleFunc("/health/all", healthAll(backends, log))
	mux.HandleFunc("/p0", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(p0HTML)
	})
	mux.HandleFunc("/approval", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(approvalHTML)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/admin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(adminHTML)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	port := envOr("BIZ_ADMIN_HTTP_PORT", "8080")
	return &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("biz-admin-web listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Error("server failed", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	})
}

// proxy 简单反代 — 把 /api/<svc>/foo 改写成 target/<foo>.
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
