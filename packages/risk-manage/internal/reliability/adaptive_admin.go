package reliability

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// RegisterAdaptiveAdmin 把 AdaptiveLimiter 的管理 endpoint 挂到 mux：
//
//	GET  /admin/reliability/limits                      → 全量快照
//	GET  /admin/reliability/limits/{merchantID}         → 单 merchant 快照
//	POST /admin/reliability/limits/{merchantID}         → {"limit": N}    强制设 limit
//	POST /admin/reliability/limits/enable               → {"enabled": bool}  在线开关
//	POST /admin/reliability/limits/reset                → 重置所有 merchant 到 Initial
//
// 所有 POST 路径假设 admin auth 中间件已在 mux 外层套（pathScopedAuth("/admin/")）。
// limiter == nil 时只挂 404 占位，避免 main.go 一定要传非空。
func RegisterAdaptiveAdmin(mux *http.ServeMux, lim *AdaptiveLimiter, logger *zap.Logger) {
	if lim == nil {
		return
	}

	// GET 全量 + GET 单 merchant + POST 单 merchant 共用 prefix handler
	mux.HandleFunc("/admin/reliability/limits", func(w http.ResponseWriter, r *http.Request) {
		// /admin/reliability/limits（精确匹配）→ 全量
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, allSnapshot(lim))
	})

	mux.HandleFunc("/admin/reliability/limits/", func(w http.ResponseWriter, r *http.Request) {
		tail := strings.TrimPrefix(r.URL.Path, "/admin/reliability/limits/")
		// 子命令：enable / reset
		switch tail {
		case "enable":
			handleEnable(w, r, lim, logger)
			return
		case "reset":
			handleReset(w, r, lim, logger)
			return
		}
		if tail == "" {
			http.NotFound(w, r)
			return
		}
		merchantID := tail
		switch r.Method {
		case http.MethodGet:
			snap := findMerchant(lim, merchantID)
			if snap == nil {
				http.Error(w, `{"error":"merchant not found"}`, http.StatusNotFound)
				return
			}
			writeJSON(w, snap)
		case http.MethodPost:
			var body struct {
				Limit int `json:"limit"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
				return
			}
			if body.Limit <= 0 {
				http.Error(w, `{"error":"limit must be > 0"}`, http.StatusBadRequest)
				return
			}
			lim.SetLimit(merchantID, body.Limit)
			logger.Info("adaptive limit overridden",
				zap.String("merchant", merchantID),
				zap.Int("limit", body.Limit))
			writeJSON(w, map[string]any{"ok": true, "merchant_id": merchantID, "limit": body.Limit})
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	})
}

func handleEnable(w http.ResponseWriter, r *http.Request, lim *AdaptiveLimiter, logger *zap.Logger) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"enabled": lim.Enabled()})
	case http.MethodPost:
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
			// 也接受 query string 兜底（curl -X POST .../enable?on=1）
			if v := r.URL.Query().Get("on"); v != "" {
				on, _ := strconv.ParseBool(v)
				lim.SetEnabled(on)
				logger.Info("adaptive limiter toggled (query)", zap.Bool("enabled", on))
				writeJSON(w, map[string]any{"ok": true, "enabled": on})
				return
			}
			http.Error(w, `{"error":"bad json; need {\"enabled\":bool}"}`, http.StatusBadRequest)
			return
		}
		lim.SetEnabled(*body.Enabled)
		logger.Info("adaptive limiter toggled", zap.Bool("enabled", *body.Enabled))
		writeJSON(w, map[string]any{"ok": true, "enabled": *body.Enabled})
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func handleReset(w http.ResponseWriter, r *http.Request, lim *AdaptiveLimiter, logger *zap.Logger) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	lim.Reset()
	logger.Info("adaptive limiter reset to initial")
	writeJSON(w, map[string]any{"ok": true})
}

type allSnapshotResp struct {
	Enabled        bool               `json:"enabled"`
	Config         AdaptiveConfig     `json:"config"`
	GlobalInflight int64              `json:"global_inflight"`
	GlobalRejected uint64             `json:"global_rejected"`
	GlobalCap      int                `json:"global_cap"`
	Merchants      []MerchantSnapshot `json:"merchants"`
}

func allSnapshot(lim *AdaptiveLimiter) allSnapshotResp {
	inflight, rejected, enabled, gc := lim.GlobalSnapshot()
	return allSnapshotResp{
		Enabled:        enabled,
		Config:         lim.Config(),
		GlobalInflight: inflight,
		GlobalRejected: rejected,
		GlobalCap:      gc,
		Merchants:      lim.Snapshot(),
	}
}

func findMerchant(lim *AdaptiveLimiter, merchantID string) *MerchantSnapshot {
	for _, s := range lim.Snapshot() {
		if s.MerchantID == merchantID {
			ss := s
			return &ss
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
