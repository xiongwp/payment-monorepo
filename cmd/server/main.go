// Package main is the REST-to-gRPC gateway for the accounting system admin API.
// It listens for HTTP requests on :9090 and forwards them to the accounting-system
// gRPC server (AccountingAdminService) running on :50051.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	accountingv1 "github.com/xiongwp/accounting-grpc-api/gen/accounting/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	grpcAddr := envOr("ACCOUNTING_GRPC_ADDR", "localhost:50051")
	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("failed to connect to accounting-system gRPC at %s: %v", grpcAddr, err)
	}
	defer conn.Close()

	adminClient := accountingv1.NewAccountingAdminServiceClient(conn)
	gw := &gateway{admin: adminClient}

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
	log.Printf("accounting-grpc-api gateway listening on :%s → gRPC %s", port, grpcAddr)
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      corsMiddleware(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}

type gateway struct {
	admin accountingv1.AccountingAdminServiceClient
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
