// http.go: extsignal webhook HTTP handler.
//
// 给外部反欺诈系统（Sift / MaxMind / 自建 batch scorer）push 分数的入口。
// 流程：
//
//   外部系统计算完分数 → POST /admin/extsignal/push
//                         Body: {provider, entity_type, entity_key, score, reasons, raw_json}
//                       → risk-manage 写 ScoreCache
//   下一笔 Screen 调 → 规则查 cache 已有最新 score → 走判定
//
// 这样把"调外部 API"从 Screen 主路径移到异步 push 模式：caller 不阻塞，
// risk-manage 也不背 SLA。厂商常见的 webhook 模式（Sift "score updated"
// notification）天然适配。
package extsignal

import (
	"encoding/json"
	"net/http"

	"go.uber.org/zap"
)

// RegisterHandlers 把 push / get 端点挂到 mux：
//
//	POST /admin/extsignal/push   外部系统 webhook 推一条
//	GET  /admin/extsignal/get?provider=sift&type=user&key=42  查缓存
//	GET  /admin/extsignal/stats  缓存大小（debug）
//	POST /admin/extsignal/purge  Body: {entity_type, entity_key}  GDPR 清
func RegisterHandlers(mux *http.ServeMux, cache *ScoreCache, logger *zap.Logger) {
	mux.HandleFunc("/admin/extsignal/push", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Provider   string   `json:"provider"`
			EntityType string   `json:"entity_type"`
			EntityKey  string   `json:"entity_key"`
			Score      float64  `json:"score"`
			Reasons    []string `json:"reasons"`
			RawJSON    string   `json:"raw_json"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.Provider == "" || body.EntityType == "" || body.EntityKey == "" {
			http.Error(w, `{"error":"provider/entity_type/entity_key required"}`, http.StatusBadRequest)
			return
		}
		if body.Score < 0 || body.Score > 1 {
			http.Error(w, `{"error":"score must be in [0, 1]"}`, http.StatusBadRequest)
			return
		}
		cache.Put(ProviderResult{
			Provider: ProviderName(body.Provider),
			Entity:   EntityKey{Type: body.EntityType, Key: body.EntityKey},
			Score:    body.Score,
			Reasons:  body.Reasons,
			RawJSON:  body.RawJSON,
		})
		if logger != nil {
			logger.Info("extsignal pushed",
				zap.String("provider", body.Provider),
				zap.String("entity", body.EntityType+":"+body.EntityKey),
				zap.Float64("score", body.Score))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/admin/extsignal/get", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		provider := r.URL.Query().Get("provider")
		entType := r.URL.Query().Get("type")
		entKey := r.URL.Query().Get("key")
		if provider == "" || entType == "" || entKey == "" {
			http.Error(w, `{"error":"provider/type/key required"}`, http.StatusBadRequest)
			return
		}
		r1, ok := cache.Get(ProviderName(provider), EntityKey{Type: entType, Key: entKey})
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hit":     ok,
			"result":  r1,
		})
	})

	mux.HandleFunc("/admin/extsignal/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"size": cache.Size(),
		})
	})

	mux.HandleFunc("/admin/extsignal/purge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			EntityType string `json:"entity_type"`
			EntityKey  string `json:"entity_key"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		n := cache.Purge(EntityKey{Type: body.EntityType, Key: body.EntityKey})
		if logger != nil {
			logger.Info("extsignal purged",
				zap.String("entity", body.EntityType+":"+body.EntityKey),
				zap.Int("removed", n))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"removed": n})
	})
}
