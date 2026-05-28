// 制裁名单 admin HTTP 端点。挂载方式（在 cmd/server 启动时调）：
//
//	sanction.RegisterAdmin(mux, svc, sanction.NewOFACDownloader(""))
//
// 提供三个 endpoint：
//
//	POST /admin/sanction/refresh  → 立即拉 OFAC（异步），返回 {"started":true}
//	GET  /admin/sanction/stats    → {entries, last_refresh, last_error, ...}
//	POST /admin/sanction/test     → Body {name,country,dob}; 跑 CheckWithFuzzy + DOB
//
// 不强求挂载；保留为独立 helper 供 main.go 选用。

package sanction

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// AdminHandler 把 downloader + MemService 绑到一组 http endpoint。
// 并发：refresh 用 inFlight 防止重复触发。
type AdminHandler struct {
	Svc        *MemService
	Downloader *OFACDownloader

	mu          sync.Mutex
	lastRefresh time.Time
	lastError   string
	inFlight    atomic.Bool
}

// RegisterAdmin 把 endpoint 挂到 mux。downloader 可为 nil（refresh 端点会返 503）。
func RegisterAdmin(mux *http.ServeMux, svc *MemService, dl *OFACDownloader) *AdminHandler {
	h := &AdminHandler{Svc: svc, Downloader: dl}
	mux.HandleFunc("/admin/sanction/refresh", h.handleRefresh)
	mux.HandleFunc("/admin/sanction/stats", h.handleStats)
	mux.HandleFunc("/admin/sanction/test", h.handleTest)
	return h
}

func (h *AdminHandler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Downloader == nil {
		http.Error(w, "downloader not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.inFlight.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusAccepted, map[string]any{"started": false, "reason": "already in flight"})
		return
	}
	go func() {
		defer h.inFlight.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		entries, fresh, err := h.Downloader.Download(ctx)
		h.mu.Lock()
		h.lastRefresh = time.Now().UTC()
		if err != nil {
			h.lastError = err.Error()
			h.mu.Unlock()
			return
		}
		h.lastError = ""
		h.mu.Unlock()
		if !fresh || len(entries) == 0 {
			return
		}
		_ = h.Svc.Reload(ctx, entries)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

func (h *AdminHandler) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n, last := h.Svc.Stats()
	h.mu.Lock()
	resp := map[string]any{
		"entries":         n,
		"last_reload_at":  last,
		"last_refresh_at": h.lastRefresh,
		"last_error":      h.lastError,
		"in_flight":       h.inFlight.Load(),
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

type testReq struct {
	Name    string `json:"name"`
	Country string `json:"country"`
	DOB     string `json:"dob"`
}

func (h *AdminHandler) handleTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req testReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	res := h.Svc.CheckWithFuzzy(r.Context(), req.Name, req.Country, 2)
	resp := map[string]any{
		"matched": res != nil,
	}
	if res != nil {
		dobLevel := "unknown"
		matchedDOB := true
		if req.DOB != "" && len(res.Hits) > 0 {
			for _, e := range res.Hits {
				ok, lvl := MatchDOB(req.DOB, e.BirthDate)
				if ok {
					dobLevel = lvl
					break
				}
				matchedDOB = false
			}
		}
		hits := make([]map[string]any, 0, len(res.Hits))
		for _, e := range res.Hits {
			hits = append(hits, map[string]any{
				"source":     e.Source,
				"uid":        e.UID,
				"name":       e.Name,
				"country":    e.Country,
				"birth_date": e.BirthDate,
			})
		}
		resp["hits"] = hits
		resp["fuzzy"] = res.Fuzzy
		resp["dob_level"] = dobLevel
		resp["dob_match"] = matchedDOB
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
