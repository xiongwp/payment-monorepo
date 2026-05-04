package feedback

import (
	"encoding/json"
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/metrics"
)

// RegisterHandlers 注册反馈端点：
//
//	POST /admin/feedback/outcome   Body: {decision_id, source, is_fraud, actor, notes}
//	GET  /admin/feedback/get?id=<decision_id>
//	GET  /admin/feedback/recent?limit=N
//
// 用法：
//   - dispute / chargeback 系统在终态时 POST 一条 outcome
//   - merchant 主动反馈 / fraud team 标记结果时 POST
//   - ML 训练 pipeline 周期 GET recent 拉数据
func RegisterHandlers(mux *http.ServeMux, rec Recorder, logger *zap.Logger) {
	mux.HandleFunc("/admin/feedback/outcome", makeRecordHandler(rec, logger))
	mux.HandleFunc("/admin/feedback/get", makeGetHandler(rec))
	mux.HandleFunc("/admin/feedback/recent", makeRecentHandler(rec))
}

func makeRecordHandler(rec Recorder, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			DecisionID string `json:"decision_id"`
			Source     string `json:"source"`
			IsFraud    bool   `json:"is_fraud"`
			Actor      string `json:"actor"`
			Notes      string `json:"notes"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if err := rec.Record(Outcome{
			DecisionID: body.DecisionID,
			Source:     Source(body.Source),
			IsFraud:    body.IsFraud,
			Actor:      body.Actor,
			Notes:      body.Notes,
		}); err != nil {
			logger.Warn("feedback record failed", zap.Error(err))
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		fraudLabel := "false"
		if body.IsFraud {
			fraudLabel = "true"
		}
		metrics.OutcomeTotal.WithLabelValues(body.Source, fraudLabel).Inc()
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func makeGetHandler(rec Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, rec.Get(id))
	}
}

func makeRecentHandler(rec Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 100
		}
		writeJSON(w, http.StatusOK, rec.Recent(limit))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
