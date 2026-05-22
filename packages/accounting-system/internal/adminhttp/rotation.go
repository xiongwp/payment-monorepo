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
	mux.HandleFunc("/admin/rotation/balance-summary", s.handleBalanceSummary)       // GET ?logical_account_key= 对账接口
	mux.HandleFunc("/admin/rotation/register", s.handleRegister)                    // POST {logical_account_key, account_type, account_business_type, currency, rotation_enabled, operator, ...}
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":             "manual switch completed",
		"logical_account_key": req.LogicalAccountKey,
		"operator":            req.Operator,
	})
}

// handleBalanceSummary GET /admin/rotation/balance-summary?logical_account_key=...
// 对账接口：返回 LA 维度的聚合余额（fleet 全 sub 之和，分组/分 phase breakdown）。
func (s *Server) handleBalanceSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rotationAdminSvc == nil {
		s.rotationAdminUnavailable(w)
		return
	}
	key := r.URL.Query().Get("logical_account_key")
	idStr := r.URL.Query().Get("logical_account_id")
	var id int64
	if idStr != "" {
		parsed, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid logical_account_id"})
			return
		}
		id = parsed
	}
	if key == "" && id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "logical_account_key or logical_account_id required"})
		return
	}
	summary, err := s.rotationAdminSvc.GetBalanceSummary(r.Context(), key, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// registerLAReq POST /admin/rotation/register 入参 — 跟 service.RegisterLogicalAccountRequest 同 shape。
type registerLAReq struct {
	LogicalAccountKey   string `json:"logical_account_key"`
	AccountType         int8   `json:"account_type"`
	AccountBusinessType int8   `json:"account_business_type"`
	Currency            string `json:"currency"`
	Description         string `json:"description,omitempty"`
	RotationEnabled     bool   `json:"rotation_enabled"`
	Operator            string `json:"operator"`
}

// handleRegister POST /admin/rotation/register —— 创建新 LogicalAccount.
// 前缀必须命中 model.AllowedKeyPrefixes (transit: / channel-payable: / channel-receivable: ...)
// key 已存在返回 409；其它错误 400 / 500。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rotationAdminSvc == nil {
		s.rotationAdminUnavailable(w)
		return
	}
	var req registerLAReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	// 业务必填校验（key/account_type/business_type/currency/operator）
	missing := []string{}
	if req.LogicalAccountKey == "" {
		missing = append(missing, "logical_account_key")
	}
	if req.AccountType == 0 {
		missing = append(missing, "account_type")
	}
	if req.AccountBusinessType == 0 {
		missing = append(missing, "account_business_type")
	}
	if req.Currency == "" {
		missing = append(missing, "currency")
	}
	if req.Operator == "" {
		missing = append(missing, "operator")
	}
	if len(missing) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "missing required fields",
			"fields": missing,
		})
		return
	}

	la, err := s.rotationAdminSvc.RegisterLogicalAccount(r.Context(), service.RegisterLogicalAccountRequest{
		LogicalAccountKey:   req.LogicalAccountKey,
		AccountType:         req.AccountType,
		AccountBusinessType: req.AccountBusinessType,
		Currency:            req.Currency,
		Description:         req.Description,
		RotationEnabled:     req.RotationEnabled,
		Operator:            req.Operator,
	})
	if err != nil {
		// key 已存在 → 409 Conflict (前端弹"已存在"提示更友好)
		// 前缀不合法 → 400
		// 其它（DB 故障等）→ 500
		msg := err.Error()
		status := http.StatusInternalServerError
		switch {
		case containsAny(msg, "already exists", "key exists", "lost race"):
			status = http.StatusConflict
		case containsAny(msg, "does not match any allowed prefix", "out of range", "non-ASCII", "required", "invalid logical_account_key"):
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":             "logical_account registered",
		"logical_account_key": la.LogicalAccountKey,
		"id":                  la.ID,
		"rotation_enabled":    la.RotationEnabled == 1,
		"registered_by":       la.RegisteredBy,
	})
}

// containsAny 小工具：判断 s 是否含任意一个 needle 子串。
// 不引入额外 import；adminhttp 已有 strings 用的话改成 strings.Contains 即可。
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if len(n) == 0 || len(s) < len(n) {
			continue
		}
		for i := 0; i+len(n) <= len(s); i++ {
			if s[i:i+len(n)] == n {
				return true
			}
		}
	}
	return false
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
