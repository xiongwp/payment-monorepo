// 04_resource_server.go — 演示 resource server 集成 payment-mw + OAuth2。
//
// 这是 *业务服务* 该长的样子：90% 都是业务逻辑，OAuth2 只是 2 行配置。
//
// 启动:
//   source .env
//   go run ./04_resource_server.go
//
// 端点:
//   GET  /healthz                   — public (no auth)
//   POST /api/v1/charges            — 需 charge:write
//   GET  /api/v1/charges            — 需 charge:read
//   POST /api/v1/refunds            — 需 refund:write
//   GET  /api/v1/refunds            — 需 refund:read
//   GET  /api/v1/whoami             — debug: 看当前 actor

//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	mw "reconcile-system/packages/payment-mw"

	"go.uber.org/zap"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	// 关键: env 配齐 → mw.Bootstrap 自动启 BearerJWT verifier
	//   OAUTH2_JWKS_URL=http://localhost:8087/.well-known/jwks.json
	//   OAUTH2_ISSUER=http://localhost:8087
	//   OAUTH2_AUDIENCE=payment-api

	mw.Bootstrap(mw.BootstrapConfig{
		ServiceName: "demo-resource-server",
		DefaultPort: "9090",
		SetupRoutes: func(mux *http.ServeMux, log *zap.Logger) error {
			// /healthz 和 /metrics 已经被 mw 自动注册 + 列为 public

			// 业务路由 + 各自的 scope 守卫
			mux.Handle("/api/v1/charges",
				routeByMethod(map[string]http.Handler{
					"POST": mw.RequireScope("charge:write")(http.HandlerFunc(createCharge)),
					"GET":  mw.RequireScope("charge:read")(http.HandlerFunc(listCharges)),
				}))

			mux.Handle("/api/v1/refunds",
				routeByMethod(map[string]http.Handler{
					"POST": mw.RequireScope("refund:write")(http.HandlerFunc(createRefund)),
					"GET":  mw.RequireScope("refund:read")(http.HandlerFunc(listRefunds)),
				}))

			// debug endpoint — 不要求 scope
			mux.HandleFunc("/api/v1/whoami", whoami)
			return nil
		},
		AuthCfg: mw.AuthConfig{
			PublicPaths: []string{"/healthz", "/metrics"},
		},
		OnStart: func(ctx context.Context, log *zap.Logger) error {
			log.Info("demo-resource-server ready",
				zap.String("token_endpoint", os.Getenv("OAUTH2_ISSUER")+"/oauth2/token"))
			return nil
		},
	})
	_ = logger
}

// ─── 业务 handlers ─────────────────────────────────────────────────────

func createCharge(w http.ResponseWriter, r *http.Request) {
	actor := mw.ActorFromCtx(r.Context())
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          fmt.Sprintf("ch_%d", time.Now().UnixNano()),
		"amount":      100,
		"currency":    "USD",
		"status":      "succeeded",
		"merchant_id": actor.MerchantID,
		"created_by":  describeActor(actor),
	})
}

func listCharges(w http.ResponseWriter, r *http.Request) {
	actor := mw.ActorFromCtx(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"data": []map[string]any{
			{"id": "ch_001", "amount": 100, "currency": "USD"},
			{"id": "ch_002", "amount": 250, "currency": "USD"},
		},
		"scoped_to": actor.MerchantID,
	})
}

func createRefund(w http.ResponseWriter, r *http.Request) {
	actor := mw.ActorFromCtx(r.Context())
	var body struct {
		ChargeID    string `json:"charge_id"`
		AmountMinor int64  `json:"amount_minor"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        fmt.Sprintf("rf_%d", time.Now().UnixNano()),
		"charge_id": body.ChargeID,
		"amount":    body.AmountMinor,
		"status":    "pending",
		"actor":     describeActor(actor),
	})
}

func listRefunds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": []any{}})
}

func whoami(w http.ResponseWriter, r *http.Request) {
	actor := mw.ActorFromCtx(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"actor_type":  actor.Type,
		"client_id":   actor.ClientID,
		"merchant_id": actor.MerchantID,
		"service_id":  actor.ServiceID,
		"ops_email":   actor.OpsEmail,
		"scopes":      actor.Scopes,
		"trace_id":    mw.TraceFromCtx(r.Context()),
	})
}

// ─── helpers ───────────────────────────────────────────────────────────

func routeByMethod(routes map[string]http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := routes[r.Method]
		if !ok {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func describeActor(a mw.Actor) string {
	switch a.Type {
	case "merchant":
		return "merchant:" + a.MerchantID
	case "service":
		return "service:" + a.ServiceID
	case "ops":
		return "ops:" + a.OpsEmail
	case "internal":
		return "internal-token"
	}
	return a.Type
}
