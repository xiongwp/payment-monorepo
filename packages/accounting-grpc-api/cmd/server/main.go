// Package main REST-to-Kitex gateway for the accounting system admin API.
// uber/fx 装配, 跟 order-core / accounting-system 同款风格.
// HTTP :9090, 转发到 accounting-system Kitex :50051 (AccountingAdminService).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/kitex/client"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	accountingadminservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingadminservice"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newAdminClient,
			newGateway,
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

// newAdminClient Kitex client 到 accounting-system AccountingAdminService.
// Kitex 自带 connection pool + keepalive, 不再需要单独 *grpc.ClientConn provider.
func newAdminClient(log *zap.Logger) (accountingadminservice.Client, error) {
	grpcAddr := envOr("ACCOUNTING_GRPC_ADDR", "localhost:50051")
	cli, err := accountingadminservice.NewClient("accounting-system",
		client.WithHostPorts(grpcAddr),
		client.WithRPCTimeout(15*time.Second),
		// TODO: client.WithResolver(kitexutil.NewEtcdResolver(etcdCli, "")) — 接 etcd
	)
	if err != nil {
		log.Error("kitex dial accounting-system failed",
			zap.String("addr", grpcAddr), zap.Error(err))
		return nil, fmt.Errorf("accountingadminservice.NewClient %s: %w", grpcAddr, err)
	}
	log.Info("accounting-system Kitex client ready", zap.String("addr", grpcAddr))
	return cli, nil
}

func newGateway(adminClient accountingadminservice.Client) *gateway {
	return &gateway{admin: adminClient}
}

func newHTTPServer(gw *gateway) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/hot-accounts/reload", gw.handleHotAccountReload)
	mux.HandleFunc("/v1/hot-accounts/", gw.handleHotAccountByID)
	mux.HandleFunc("/v1/hot-accounts", gw.handleHotAccounts)
	mux.HandleFunc("/v1/buffer-accounts/reload", gw.handleBufferAccountReload)
	mux.HandleFunc("/v1/buffer-accounts/", gw.handleBufferAccountByID)
	mux.HandleFunc("/v1/buffer-accounts", gw.handleBufferAccounts)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]string{"status": "ok"})
	})

	port := envOr("PORT", "9090")
	return &http.Server{
		Addr:         ":" + port,
		Handler:      corsMiddleware(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("accounting-grpc-api gateway listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Error("HTTP server failed", zap.Error(err))
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

type gateway struct {
	admin accountingadminservice.Client
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Hot account handlers ─────────────────────────────────────────────────────

func (g *gateway) handleHotAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := g.admin.ListHotAccounts(ctx, &accountingv1.ListHotAccountsRequest{})
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if resp.Code != 0 {
			jsonError(w, resp.Message, http.StatusInternalServerError)
			return
		}
		jsonOK(w, resp.Items)

	case http.MethodPost:
		var req struct {
			AccountNo   string `json:"account_no"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := g.admin.CreateHotAccount(ctx, &accountingv1.CreateHotAccountRequest{
			AccountNo:   req.AccountNo,
			Description: req.Description,
		})
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if resp.Code != 0 {
			jsonError(w, resp.Message, grpcCodeToHTTP(int(resp.Code)))
			return
		}
		jsonOK(w, resp.Item)

	default:
		methodNotAllowed(w)
	}
}

func (g *gateway) handleHotAccountByID(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/v1/hot-accounts/")
	if idStr == "reload" {
		g.handleHotAccountReload(w, r)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Enabled     bool   `json:"enabled"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := g.admin.UpdateHotAccount(ctx, &accountingv1.UpdateHotAccountRequest{
			Id:          id,
			Enabled:     req.Enabled,
			Description: req.Description,
		})
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if resp.Code != 0 {
			jsonError(w, resp.Message, grpcCodeToHTTP(int(resp.Code)))
			return
		}
		jsonOK(w, map[string]string{"message": "updated"})

	case http.MethodDelete:
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := g.admin.DeleteHotAccount(ctx, &accountingv1.DeleteHotAccountRequest{Id: id})
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if resp.Code != 0 {
			jsonError(w, resp.Message, grpcCodeToHTTP(int(resp.Code)))
			return
		}
		jsonOK(w, map[string]string{"message": "deleted"})

	default:
		methodNotAllowed(w)
	}
}

func (g *gateway) handleHotAccountReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := g.admin.ReloadHotAccountAllowlist(ctx, &accountingv1.ReloadHotAccountAllowlistRequest{})
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if resp.Code != 0 {
		jsonError(w, resp.Message, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]interface{}{"message": "reloaded", "count": resp.AccountCount})
}

// ─── Buffer account handlers ──────────────────────────────────────────────────

func (g *gateway) handleBufferAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := g.admin.ListBufferAccounts(ctx, &accountingv1.ListBufferAccountsRequest{})
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if resp.Code != 0 {
			jsonError(w, resp.Message, http.StatusInternalServerError)
			return
		}
		jsonOK(w, resp.Items)

	case http.MethodPost:
		var req struct {
			AccountNo          string `json:"account_no"`
			FlushIntervalLevel int32  `json:"flush_interval_level"`
			Description        string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := g.admin.CreateBufferAccount(ctx, &accountingv1.CreateBufferAccountRequest{
			AccountNo:          req.AccountNo,
			FlushIntervalLevel: req.FlushIntervalLevel,
			Description:        req.Description,
		})
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if resp.Code != 0 {
			jsonError(w, resp.Message, grpcCodeToHTTP(int(resp.Code)))
			return
		}
		jsonOK(w, resp.Item)

	default:
		methodNotAllowed(w)
	}
}

func (g *gateway) handleBufferAccountByID(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/v1/buffer-accounts/")
	if idStr == "reload" {
		g.handleBufferAccountReload(w, r)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Enabled            bool   `json:"enabled"`
			FlushIntervalLevel int32  `json:"flush_interval_level"`
			Description        string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := g.admin.UpdateBufferAccount(ctx, &accountingv1.UpdateBufferAccountRequest{
			Id:                 id,
			Enabled:            req.Enabled,
			FlushIntervalLevel: req.FlushIntervalLevel,
			Description:        req.Description,
		})
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if resp.Code != 0 {
			jsonError(w, resp.Message, grpcCodeToHTTP(int(resp.Code)))
			return
		}
		jsonOK(w, map[string]string{"message": "updated"})

	case http.MethodDelete:
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := g.admin.DeleteBufferAccount(ctx, &accountingv1.DeleteBufferAccountRequest{Id: id})
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if resp.Code != 0 {
			jsonError(w, resp.Message, grpcCodeToHTTP(int(resp.Code)))
			return
		}
		jsonOK(w, map[string]string{"message": "deleted"})

	default:
		methodNotAllowed(w)
	}
}

func (g *gateway) handleBufferAccountReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := g.admin.ReloadBufferAccountConfig(ctx, &accountingv1.ReloadBufferAccountConfigRequest{})
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if resp.Code != 0 {
		jsonError(w, resp.Message, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]interface{}{"message": "reloaded", "count": resp.AccountCount})
}

// ─── Response helpers ─────────────────────────────────────────────────────────

type apiResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

func jsonOK(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apiResponse{Code: 0, Data: data})
}

func jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{Code: status, Message: msg})
}

func methodNotAllowed(w http.ResponseWriter) {
	jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
}

func grpcCodeToHTTP(code int) int {
	if code == 400 {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
