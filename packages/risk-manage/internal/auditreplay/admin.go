// admin.go — replay admin endpoints + rerun-with-current-rules diff。
package auditreplay

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Reevaluator 用当前规则集 + ML 模型对历史 snapshot 重跑，返回新 verdict。
//
// 由 cmd/server/main.go wire 时把真实的 risk.Service.RerunFromSnapshot 注入。
type Reevaluator interface {
	Rerun(ctx context.Context, snap SignalSnapshot) (newVerdict string, newScore int, newHits []string, err error)
}

// AdminHandlers HTTP routes：
//
//	GET  /admin/replay/{decision_id}              拿原始信号
//	POST /admin/replay/{decision_id}/rerun        当前规则重跑 → diff
//	GET  /admin/replay/list?date=2026-05-20&limit=100
//	POST /admin/replay/purge?days=30              清理超 30 天
func AdminHandlers(store Store, reeval Reevaluator) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/admin/replay/", func(w http.ResponseWriter, r *http.Request) {
		// 拆 /admin/replay/{decision_id}[/rerun]
		path := r.URL.Path[len("/admin/replay/"):]
		if path == "" {
			http.NotFound(w, r)
			return
		}
		// list / purge 走单独前缀分支
		if path == "list" || path == "purge" {
			http.NotFound(w, r) // 走下面注册的精确 handler
			return
		}
		decisionID := path
		rerun := false
		if idx := lastSlash(path); idx > 0 && path[idx+1:] == "rerun" {
			decisionID = path[:idx]
			rerun = true
		}

		snap, err := store.Get(r.Context(), decisionID)
		if err != nil {
			http.Error(w, "snapshot not found", http.StatusNotFound)
			return
		}

		if !rerun {
			writeJSON(w, snap)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST required for rerun", http.StatusMethodNotAllowed)
			return
		}
		if reeval == nil {
			http.Error(w, "reevaluator not configured", http.StatusServiceUnavailable)
			return
		}
		newV, newS, newHits, err := reeval.Rerun(r.Context(), snap)
		if err != nil {
			http.Error(w, "rerun failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]interface{}{
			"decision_id":   decisionID,
			"old_verdict":   snap.Verdict,
			"old_score":     snap.Score,
			"old_hits":      snap.Hits,
			"old_rule_ver":  snap.RuleVersion,
			"old_model_ver": snap.ModelVer,
			"new_verdict":   newV,
			"new_score":     newS,
			"new_hits":      newHits,
			"verdict_diff":  snap.Verdict != newV,
		})
	})

	mux.HandleFunc("/admin/replay/list", func(w http.ResponseWriter, r *http.Request) {
		dateStr := r.URL.Query().Get("date")
		limitStr := r.URL.Query().Get("limit")
		if dateStr == "" {
			http.Error(w, "?date=YYYY-MM-DD required", http.StatusBadRequest)
			return
		}
		date, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			http.Error(w, "bad date", http.StatusBadRequest)
			return
		}
		limit := 100
		if limitStr != "" {
			if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}
		snaps, err := store.List(r.Context(), date, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]interface{}{"count": len(snaps), "items": snaps})
	})

	mux.HandleFunc("/admin/replay/purge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		days := 30
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n >= 1 && n <= 3650 {
				days = n
			}
		}
		// 只有 LocalStore 有 Purge；其他 Store 提供 sentinel
		type purger interface {
			PurgeOlderThan(ctx context.Context, d time.Duration) (int, error)
		}
		if p, ok := store.(purger); ok {
			deleted, err := p.PurgeOlderThan(r.Context(), time.Duration(days)*24*time.Hour)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]interface{}{"deleted": deleted, "older_than_days": days})
			return
		}
		http.Error(w, "store does not support purge", http.StatusNotImplemented)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
