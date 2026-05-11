// Package adminhttp — payment-gateway 对外 HTTP。
//
//   POST /api/v1/tokens          tokenize 卡 (商户 SDK 调)
//   POST /api/v1/route           智能路由决策 (内部调试 / 商户 dry-run)
//   POST /api/v1/charge          统一 charge endpoint — 内部 route + tokenize + 调通道
//                                (生产真接通道 SDK，这里只 stub 返决策)

package adminhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/payment-gateway/internal/routing"
	"reconcile-system/packages/payment-gateway/internal/tokenize"
)

type Server struct {
	tok    *tokenize.Service
	router *routing.Router
	log    *zap.Logger
}

func New(tok *tokenize.Service, router *routing.Router, log *zap.Logger) *Server {
	return &Server{tok: tok, router: router, log: log}
}

func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/tokens", s.createToken)
	mux.HandleFunc("/api/v1/route", s.routeDecide)
	mux.HandleFunc("/api/v1/charge", s.unifiedCharge)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
}

// createToken POST /api/v1/tokens
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Card       tokenize.CardInput `json:"card"`
		Type       string             `json:"type"`        // one_time / reusable
		CustomerID string             `json:"customer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ttype := tokenize.TokenOneTime
	if body.Type == string(tokenize.TokenReusable) {
		ttype = tokenize.TokenReusable
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tok, err := s.tok.Tokenize(ctx, body.Card, ttype, body.CustomerID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, tok)
}

// routeDecide POST /api/v1/route
func (s *Server) routeDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req routing.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	dec, err := s.router.Decide(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, dec)
}

// unifiedCharge POST /api/v1/charge — 统一 charge 入口。
//
// 真实流程:
//   1. router.Decide → 选 primary 通道
//   2. tokenize.Detokenize → 拿 PAN（如果是 token charge）
//   3. 调通道 SDK 真扣款
//   4. 失败 → 走 fallback 通道
//   5. 返回 charge_id + status
//
// MVP: 只做 1+2，3 用 stub 返成功。
func (s *Server) unifiedCharge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		TokenID        string `json:"token_id"`
		MerchantID     string `json:"merchant_id"`
		AmountMinor    int64  `json:"amount_minor"`
		Currency       string `json:"currency"`
		Region         string `json:"region"`
		Product        string `json:"product"`
		MerchantPrefs  []string `json:"merchant_prefs,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 1. detokenize 拿 BIN
	bin := ""
	if body.TokenID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		pan, _, err := s.tok.Detokenize(ctx, body.TokenID, "charge")
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("detokenize: %w", err))
			return
		}
		if len(pan) >= 6 {
			bin = pan[:6]
		}
	}
	// 2. route
	dec, err := s.router.Decide(routing.Request{
		MerchantID: body.MerchantID, AmountMinor: body.AmountMinor,
		Currency: body.Currency, Region: body.Region,
		CardBIN: bin, Product: body.Product, MerchantPrefs: body.MerchantPrefs,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 3. stub charge (生产换通道 SDK)
	writeJSON(w, http.StatusOK, map[string]any{
		"charge_id":         "ch_" + body.MerchantID + "_" + body.TokenID,
		"status":            "succeeded",
		"primary_channel":   dec.Primary.ID,
		"fallback_channels": fallbackIDs(dec),
		"amount_minor":      body.AmountMinor,
		"currency":          body.Currency,
	})
}

func fallbackIDs(d *routing.Decision) []string {
	out := make([]string, len(d.Fallback))
	for i, c := range d.Fallback {
		out[i] = c.ID
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}
