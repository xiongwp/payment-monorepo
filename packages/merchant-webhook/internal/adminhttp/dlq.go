// dlq.go — DLQ + attempt 历史 HTTP 路由.
//
//   GET  /api/v1/dlq?limit=&offset=                  Admin: DLQ deliveries list
//   POST /api/v1/dlq/replay   { "delivery_ids":[...] }  Admin: replay
//   GET  /api/v1/me/deliveries/{id}/attempts         商户/Admin: 历史尝试
//   GET  /api/v1/me/deliveries/{id}                  商户: 单个 delivery 详情
//
// 鉴权: /api/v1/dlq/* 用 X-Admin-Token; /api/v1/me/* 用 X-Merchant-Id (oauth2 后塞 jwt sub).

package adminhttp

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// MountDLQ 把 DLQ + attempt 历史路由挂上.
func (s *Server) MountDLQ(mux *http.ServeMux, adminToken string) {
	mux.HandleFunc("/api/v1/dlq", s.requireAdmin(adminToken, s.dlqListHandler))
	mux.HandleFunc("/api/v1/dlq/replay", s.requireAdmin(adminToken, s.dlqReplayHandler))
	mux.HandleFunc("/api/v1/me/deliveries/", s.deliveryDetailHandler)
}

func (s *Server) dlqListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	list, err := s.repo.ListDLQ(r.Context(), limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deliveries": list,
		"count":      len(list),
		"limit":      limit,
		"offset":     offset,
	})
}

func (s *Server) dlqReplayHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		DeliveryIDs []int64 `json:"delivery_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(body.DeliveryIDs) == 0 {
		writeErr(w, http.StatusBadRequest, errStr("delivery_ids required"))
		return
	}
	n, err := s.repo.ReplayDLQ(r.Context(), body.DeliveryIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replayed": n})
}

// /api/v1/me/deliveries/{id}            GET delivery 详情
// /api/v1/me/deliveries/{id}/attempts   GET 历史尝试
func (s *Server) deliveryDetailHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/me/deliveries/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, http.StatusBadRequest, errStr("missing delivery_id"))
		return
	}
	delID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errStr("bad delivery_id"))
		return
	}
	del, err := s.repo.GetDelivery(r.Context(), delID)
	if err != nil || del == nil {
		writeErr(w, http.StatusNotFound, errStr("not found"))
		return
	}
	// 商户 ownership 校验 — X-Merchant-Id 比 endpoint.MerchantID
	mid := r.Header.Get("X-Merchant-Id")
	if mid != "" {
		ep, _ := s.repo.GetEndpoint(r.Context(), del.EndpointID)
		if ep == nil || ep.MerchantID != mid {
			writeErr(w, http.StatusForbidden, errStr("not your delivery"))
			return
		}
	}
	if len(parts) > 1 && parts[1] == "attempts" {
		atts, _ := s.repo.ListAttempts(r.Context(), delID)
		writeJSON(w, http.StatusOK, map[string]any{
			"delivery": del,
			"attempts": atts,
		})
		return
	}
	writeJSON(w, http.StatusOK, del)
}

// requireAdmin 中间件 — 检查 X-Admin-Token.
func (s *Server) requireAdmin(adminToken string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminToken == "" {
			h(w, r)
			return
		}
		if r.Header.Get("X-Admin-Token") != adminToken {
			writeErr(w, http.StatusForbidden, errStr("admin token required"))
			return
		}
		h(w, r)
	}
}

type errStringT struct{ s string }

func (e errStringT) Error() string { return e.s }
func errStr(s string) error        { return errStringT{s: s} }
