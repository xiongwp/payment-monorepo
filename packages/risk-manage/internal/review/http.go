package review

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// RegisterHandlers 把 admin review 端点注册到 mux：
//
//	GET  /admin/review/list?status=pending&limit=100&offset=0
//	GET  /admin/review/get?id=<decision_id>
//	POST /admin/review/decide  Body: {id, action, actor, reason}
//
// 这些端点假定调用方（admin-web）已经在 api-gateway 层做了人员鉴权 +
// 审计 actor 注入；本 handler 不再独立 auth（只信任入站 actor 字段）。
func RegisterHandlers(mux *http.ServeMux, store Store, logger *zap.Logger) {
	RegisterHandlersWithHook(mux, store, logger, nil)
}

// RegisterHandlersWithHook 同 RegisterHandlers，但允许在 decide 成功后调用
// onDecided 回调（如：把决议结果写到 feedback.Recorder 做反馈闭环）。
// onDecided==nil 等价 RegisterHandlers。
//
// 全部端点：
//
//	GET  /admin/review/list?status=pending&limit=100&offset=0
//	GET  /admin/review/get?id=<decision_id>
//	GET  /admin/review/by-assignee?actor=<id>&status=in_review
//	GET  /admin/review/overdue?limit=100
//	POST /admin/review/claim    {id, actor}
//	POST /admin/review/release  {id, actor}
//	POST /admin/review/escalate {id, actor, reason}
//	POST /admin/review/note     {id, actor, body}
//	POST /admin/review/decide   {id, action, actor, reason}
func RegisterHandlersWithHook(mux *http.ServeMux, store Store, logger *zap.Logger, onDecided func(*Item)) {
	mux.HandleFunc("/admin/review/list", makeListHandler(store))
	mux.HandleFunc("/admin/review/get", makeGetHandler(store))
	mux.HandleFunc("/admin/review/by-assignee", makeByAssigneeHandler(store))
	mux.HandleFunc("/admin/review/overdue", makeOverdueHandler(store))
	mux.HandleFunc("/admin/review/claim", makeClaimHandler(store, logger))
	mux.HandleFunc("/admin/review/release", makeReleaseHandler(store, logger))
	mux.HandleFunc("/admin/review/escalate", makeEscalateHandler(store, logger))
	mux.HandleFunc("/admin/review/note", makeNoteHandler(store, logger))
	mux.HandleFunc("/admin/review/decide", makeDecideHandler(store, logger, onDecided))
	mux.HandleFunc("/admin/review/decide-bulk", makeDecideBulkHandler(store, logger, onDecided))
}

func makeByAssigneeHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		actor := q.Get("actor")
		if actor == "" {
			http.Error(w, `{"error":"actor required"}`, http.StatusBadRequest)
			return
		}
		var statuses []Status
		if s := q.Get("status"); s != "" {
			statuses = []Status{Status(s)}
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		writeJSON(w, http.StatusOK, store.ListByAssignee(actor, statuses, limit))
	}
}

func makeOverdueHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		writeJSON(w, http.StatusOK, store.OverdueSLA(time.Now(), limit))
	}
}

func makeClaimHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return mutationHandler(logger, "claim", func(body mutationBody) (*Item, error) {
		return store.Claim(body.ID, body.Actor)
	})
}

func makeReleaseHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return mutationHandler(logger, "release", func(body mutationBody) (*Item, error) {
		return store.Release(body.ID, body.Actor)
	})
}

func makeEscalateHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return mutationHandler(logger, "escalate", func(body mutationBody) (*Item, error) {
		return store.Escalate(body.ID, body.Actor, body.Reason)
	})
}

func makeNoteHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return mutationHandler(logger, "note", func(body mutationBody) (*Item, error) {
		if body.Body == "" {
			return nil, errors.New("body required")
		}
		return store.AddNote(body.ID, body.Actor, body.Body)
	})
}

type mutationBody struct {
	ID     string `json:"id"`
	Actor  string `json:"actor"`
	Reason string `json:"reason,omitempty"`
	Body   string `json:"body,omitempty"`
}

// mutationHandler 抽出几个 case 写操作的公共 boilerplate（POST + json + ID/actor 必填 + 错误映射）。
func mutationHandler(logger *zap.Logger, op string, fn func(mutationBody) (*Item, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body mutationBody
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.ID == "" || body.Actor == "" {
			http.Error(w, `{"error":"id+actor required"}`, http.StatusBadRequest)
			return
		}
		it, err := fn(body)
		if err != nil {
			switch {
			case errors.Is(err, ErrAlreadyClaimed):
				http.Error(w, `{"error":"already claimed"}`, http.StatusConflict)
			case errors.Is(err, ErrNotAssignee):
				http.Error(w, `{"error":"not assignee"}`, http.StatusForbidden)
			case errors.Is(err, ErrNotPending):
				http.Error(w, `{"error":"item not in modifiable state"}`, http.StatusConflict)
			default:
				logger.Warn("review op failed", zap.String("op", op), zap.Error(err))
				http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			}
			return
		}
		writeJSON(w, http.StatusOK, it)
	}
}

func makeListHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		status := Status(q.Get("status"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		items := store.List(status, limit, offset)
		writeJSON(w, http.StatusOK, items)
	}
}

func makeGetHandler(store Store) http.HandlerFunc {
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
		it := store.Get(id)
		if it == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, it)
	}
}

func makeDecideHandler(store Store, logger *zap.Logger, onDecided func(*Item)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ID     string `json:"id"`
			Action string `json:"action"`
			Actor  string `json:"actor"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.ID == "" || body.Action == "" || body.Actor == "" {
			http.Error(w, `{"error":"id+action+actor required"}`, http.StatusBadRequest)
			return
		}
		it, err := store.Decide(body.ID, Action(body.Action), body.Actor, body.Reason)
		if err != nil {
			if errors.Is(err, ErrNotPending) {
				http.Error(w, `{"error":"item not pending"}`, http.StatusConflict)
				return
			}
			logger.Warn("review decide failed", zap.Error(err))
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		if onDecided != nil {
			onDecided(it)
		}
		writeJSON(w, http.StatusOK, it)
	}
}

// makeDecideBulkHandler 批量审核：同一 action / actor / reason 应用到 N 笔
// 待审。常见场景：卡测试攻击下 50+ 笔同模式 review，运营一次性 reject。
//
// Body: {ids: [...], action, actor, reason}
//
// 返回 per-id 结果：success / not_pending / error。失败的不影响其它继续跑。
//
// 限制 ids ≤ 1000 防一次卡死服务。
func makeDecideBulkHandler(store Store, logger *zap.Logger, onDecided func(*Item)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			IDs    []string `json:"ids"`
			Action string   `json:"action"`
			Actor  string   `json:"actor"`
			Reason string   `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.Action == "" || body.Actor == "" {
			http.Error(w, `{"error":"action+actor required"}`, http.StatusBadRequest)
			return
		}
		if len(body.IDs) == 0 {
			http.Error(w, `{"error":"ids required"}`, http.StatusBadRequest)
			return
		}
		if len(body.IDs) > 1000 {
			http.Error(w, `{"error":"max 1000 ids per bulk call"}`, http.StatusBadRequest)
			return
		}
		type result struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error,omitempty"`
		}
		results := make([]result, 0, len(body.IDs))
		successCount := 0
		failCount := 0
		notPendingCount := 0
		for _, id := range body.IDs {
			if id == "" {
				continue
			}
			it, err := store.Decide(id, Action(body.Action), body.Actor, body.Reason)
			if err != nil {
				if errors.Is(err, ErrNotPending) {
					results = append(results, result{ID: id, Status: "not_pending"})
					notPendingCount++
				} else {
					results = append(results, result{ID: id, Status: "error", Error: err.Error()})
					failCount++
				}
				continue
			}
			if onDecided != nil {
				onDecided(it)
			}
			results = append(results, result{ID: id, Status: "success"})
			successCount++
		}
		logger.Info("review bulk decide",
			zap.String("actor", body.Actor),
			zap.String("action", body.Action),
			zap.Int("success", successCount),
			zap.Int("not_pending", notPendingCount),
			zap.Int("failed", failCount))
		writeJSON(w, http.StatusOK, map[string]any{
			"total":       len(body.IDs),
			"success":     successCount,
			"not_pending": notPendingCount,
			"failed":      failCount,
			"results":     results,
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
