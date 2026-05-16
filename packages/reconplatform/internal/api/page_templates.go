// page_templates.go — FEAT-4: 规则模板 API.
//
// 端点:
//   GET /api/v1/templates  → [{id, name, description, severity, schedule, triggers, code, tags}, ...]
//
// 数据源: seed.AllTemplates() — 把 8 条内建规则连同源码 + tags 一并返回.
// 用户在 admin web "+ 新建规则" modal 里选模板 → 把 code 灌进编辑器.
package api

import (
	"fmt"
	"net/http"

	"reconcile-system/internal/catalog/seed"
)

// templatesAPI GET /api/v1/templates.
func (s *Server) templatesAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tpls, err := seed.AllTemplates()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("load templates: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, tpls)
}
