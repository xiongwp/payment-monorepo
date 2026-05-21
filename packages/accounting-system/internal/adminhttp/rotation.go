// rotation.go — adminhttp 中的轮换账户管理端点
//
// 这些端点供 accounting-admin-web 调用，对应 internal/service/rotation_admin_service.go
// 中的 5 个方法（ListCurrentActiveInstances / GetInstanceHistory / GetInstanceDetail
// / ManualSwitch / ManualProvision）。
//
// 设计：
//   - rotationAdminSvc 字段在 NewServer 之后通过 WithRotationAdmin 注入；
//     若未注入（如 fx 还没加 provider），端点统一返回 503 "rotation admin not wired"
//     而不是 panic，保证服务可启动。
//   - 路由前缀统一 /admin/rotation/*。
package adminhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/xiongwp/accounting-system/internal/service"
)

// WithRotationAdmin 注入 rotation admin service（在 NewServer 之后调用）。
// nil 安全：传 nil 等同于不启用 rotation 端点。
func (s *Server) WithRotationAdmin(svc *service.AdminService) *Server {
	s.rotationAdminSvc = svc
	return s
}

// registerRotationRoutes 在 NewServer 内被调用，把 rotation 端点接入 mux。
// 调用方应该传入 NewServer 用的同一个 mux。
func (s *Server) registerRotationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/rotation/logical-accounts", s.handleListLogicalAccounts) // GET ?prefix=&limit=
	mux.HandleFunc("/admin/rotation/instance-history", s.handleInstanceHistory)     // GET ?logical_account_key=
	mux.HandleFunc("/admin/rotation/instance-detail", s.handleInstanceDetail)       // GET ?account_no=
	mux.HandleFunc("/admin/rotation/manual-switch", s.handleManualSwitch)           // POST {logical_account_key, operator, reason}
	mux.HandleFunc("/admin/rotation/manual-provision", s.handleManualProvision)     // POST {logical_account_key, operator, reason}
}

// rotationAdminUnavailable 写 503 + 提示信息。
func (s *Server) rotationAdminUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "rotation admin service not wired in this build; configure service.NewAdminService in fx graph",
	})
}

// handleListLogicalAccounts GET /admin/rotation/logical-accounts?prefix=...&limit=...
func (s *Server) handleListLogicalAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rotationAdminSvc == nil {
		s.rotationAdminUnavailable(w)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.rotationAdminSvc.ListCurrentActiveInstances(r.Context(), prefix, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// 用 wrapper 保持 JSON 结构稳定（rows 字段一定存在）
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":  rows,
		"count": len(rows),
	})
}

// handleInstanceHistory GET /admin/rotation/instance-history?logical_account_key=...
func (s *Server) handleInstanceHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rotationAdminSvc == nil {
		s.rotationAdminUnavailable(w)
		return
	}
	key := r.URL.Query().Get("logical_account_key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "logical_account_key required"})
		return
	}
	view, err := s.rotationAdminSvc.GetInstanceHistory(r.Context(), key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleInstanceDetail GET /admin/rotation/instance-detail?account_no=...
func (s *Server) handleInstanceDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rotationAdminSvc == nil {
		s.rotationAdminUnavailable(w)
		return
	}
	accountNo := r.URL.Query().Get("account_no")
	if accountNo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account_no required"})
		return
	}
	detail, err := s.rotationAdminSvc.GetInstanceDetail(r.Context(), accountNo)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// manualOpReq POST {logical_account_key, operator, reason}
type manualOpReq struct {
	LogicalAccountKey string `json:"logical_account_key"`
	Operator          string `json:"operator"`
	Reason            string `json:"reason"`
}

// handleManualSwitch POST /admin/rotation/manual-switch
func (s *Server) handleManualSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rotationAdminSvc == nil {
		s.rotationAdminUnavailable(w)
		return
	}
	var req manualOpReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.LogicalAccountKey == "" || req.Operator == "" || req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "logical_account_key, operator, reason are all required",
		})
		return
	}
	err := s.rotationAdminSvc.ManualSwitch(r.Context(), service.ManualSwitchRequest{
		LogicalAccountKey: req.LogicalAccountKey,
		Operator:          req.Operator,
		Reason:            req.Reason,
	})
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, http.ErrAbortHandler) { // placeholder; no specific err types yet
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":             "manual switch completed",
		"logical_account_key": req.LogicalAccountKey,
		"operator":            req.Operator,
	})
}

// handleManualProvision POST /admin/rotation/manual-provision
func (s *Server) handleManualProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rotationAdminSvc == nil {
		s.rotationAdminUnavailable(w)
		return
	}
	var req manualOpReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.LogicalAccountKey == "" || req.Operator == "" || req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "logical_account_key, operator, reason are all required",
		})
		return
	}
	err := s.rotationAdminSvc.ManualProvision(r.Context(), service.ManualSwitchRequest{
		LogicalAccountKey: req.LogicalAccountKey,
		Operator:          req.Operator,
		Reason:            req.Reason,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":             "manual provision completed",
		"logical_account_key": req.LogicalAccountKey,
		"operator":            req.Operator,
	})
}
