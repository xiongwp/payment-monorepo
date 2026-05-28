package review

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/xiongwp/risk-manage/internal/auth"
	"go.uber.org/zap"
)

// resolveActor 强制从 ctx 拿真实 actor（OIDC sub / email），不再信任请求 body
// 里的 actor 字段或 X-Actor header。前端 localStorage 输入框（Workbench）应当
// 移除：actor 由 SSO 中间件注入，前端伪造无效。
//
// OIDC 未启用时（dev / 测试），ctx 没 ActorInfo → 退而求其次用 body 字段，
// 但生产部署必须开 admin.oidc.enabled。
//
// 返空字符串 → handler 应 403 拒绝。
func resolveActor(r *http.Request, fallback string) string {
	if a := auth.ActorFromContext(r.Context()); a != nil && a.UserID != "" {
		return a.ActorString()
	}
	return fallback
}

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
	mux.HandleFunc("/admin/review/transfer", makeTransferHandler(store, logger))
	mux.HandleFunc("/admin/review/escalate-level", makeEscalateLevelHandler(store, logger))
	mux.HandleFunc("/admin/review/reason-codes", makeReasonCodesHandler())
}

// makeReasonCodesHandler GET /admin/review/reason-codes → ReasonCodeLabel[]
//
// 给 admin-web 拉下拉项用。无鉴权（labels 是公开的 enum 字典，不含敏感数据）。
// 静态数据可上层 CDN 缓存（前端也可以本地缓存一天）。
func makeReasonCodesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, ReasonCodes())
	}
}

// makeTransferHandler POST /admin/review/transfer
//
// Body: {case_id, to_actor, reason}
//
// from_actor 来自 ctx (OIDC) — 不允许 body 传，防越权改派别人的 case。
// 状态机：in_review → in_review（assigned_to 改）。
func makeTransferHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			CaseID    string `json:"case_id"`
			ToActor   string `json:"to_actor"`
			Reason    string `json:"reason"`
			FromActor string `json:"from_actor,omitempty"` // dev fallback only
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		fromActor := resolveActor(r, body.FromActor)
		if body.CaseID == "" || body.ToActor == "" {
			http.Error(w, `{"error":"case_id+to_actor required"}`, http.StatusBadRequest)
			return
		}
		if fromActor == "" {
			http.Error(w, `{"error":"no actor; enable OIDC or pass body.from_actor in dev"}`, http.StatusForbidden)
			return
		}
		it, err := store.Transfer(body.CaseID, fromActor, body.ToActor, body.Reason)
		if err != nil {
			switch {
			case errors.Is(err, ErrNotAssignee):
				http.Error(w, `{"error":"not assignee"}`, http.StatusForbidden)
			case errors.Is(err, ErrNotPending):
				http.Error(w, `{"error":"item not in_review"}`, http.StatusConflict)
			default:
				logger.Warn("review transfer failed", zap.Error(err))
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			}
			return
		}
		writeJSON(w, http.StatusOK, it)
	}
}

// makeEscalateLevelHandler POST /admin/review/escalate-level
//
// Body: {case_id, target_level, reason}
//
// 区别于旧 /admin/review/escalate：那个只是 "in_review → escalated"（bump
// 计数）。这个是真正分级升级：level=1 → level=2，trigger=manual。
// 调用方必须是当前 assignee 或拥有 admin override 权限（生产里在 api-gateway
// 校验；这里仅做状态机校验）。
func makeEscalateLevelHandler(store Store, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			CaseID      string `json:"case_id"`
			TargetLevel int    `json:"target_level"`
			Reason      string `json:"reason"`
			Actor       string `json:"actor,omitempty"` // dev fallback only
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		actor := resolveActor(r, body.Actor)
		if body.CaseID == "" || body.TargetLevel == 0 {
			http.Error(w, `{"error":"case_id+target_level required"}`, http.StatusBadRequest)
			return
		}
		if actor == "" {
			http.Error(w, `{"error":"no actor; enable OIDC or pass body.actor in dev"}`, http.StatusForbidden)
			return
		}
		it, err := store.EscalateTo(body.CaseID, body.TargetLevel, actor, body.Reason, EscalateTriggerManual)
		if err != nil {
			switch {
			case errors.Is(err, ErrInvalidLevel):
				http.Error(w, `{"error":"invalid target level (only L2 supported)"}`, http.StatusBadRequest)
			case errors.Is(err, ErrAlreadyAtLevel):
				http.Error(w, `{"error":"already at or above target level"}`, http.StatusConflict)
			case errors.Is(err, ErrNotPending):
				http.Error(w, `{"error":"case not modifiable"}`, http.StatusConflict)
			default:
				logger.Warn("review escalate-level failed", zap.Error(err))
				http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			}
			return
		}
		logger.Info("review escalate-level ok",
			zap.String("case_id", body.CaseID),
			zap.String("actor", actor),
			zap.Int("target_level", body.TargetLevel))
		writeJSON(w, http.StatusOK, it)
	}
}

func makeByAssigneeHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		// 只读 list-by-assignee 允许 query actor（admin 看别人的队列）；如果
		// OIDC actor 存在且 q.actor 为空 → 默认拿自己的。
		actor := q.Get("actor")
		if actor == "" {
			actor = resolveActor(r, "")
		}
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
		// 强制 ctx actor（OIDC sub/email），fallback 到 body.Actor 仅 dev 模式有效
		body.Actor = resolveActor(r, body.Actor)
		if body.ID == "" {
			http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
			return
		}
		if body.Actor == "" {
			http.Error(w, `{"error":"no actor; enable OIDC or pass body.actor in dev"}`, http.StatusForbidden)
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
			ID         string `json:"id"`
			Action     string `json:"action"`
			Actor      string `json:"actor"`
			Reason     string `json:"reason"`
			ReasonCode string `json:"reason_code"` // v2 必传；旧客户端临时 fallback 见下
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		body.Actor = resolveActor(r, body.Actor)
		if body.ID == "" || body.Action == "" {
			http.Error(w, `{"error":"id+action required"}`, http.StatusBadRequest)
			return
		}
		if body.Actor == "" {
			http.Error(w, `{"error":"no actor; enable OIDC or pass body.actor in dev"}`, http.StatusForbidden)
			return
		}
		// v2 强制 reason_code；非法 / 空 → 400。
		// 注意：这是 breaking change，旧 admin-web 客户端必须升级。先 grep
		// 调用方再上：payment-admin-web / dispute-svc。
		if !ValidReasonCode(body.ReasonCode) {
			http.Error(w, `{"error":"reason_code required (call GET /admin/review/reason-codes for valid values)"}`, http.StatusBadRequest)
			return
		}
		it, err := store.DecideWithCode(body.ID, Action(body.Action), body.Actor, body.ReasonCode, body.Reason)
		if err != nil {
			switch {
			case errors.Is(err, ErrInvalidReason):
				http.Error(w, `{"error":"invalid reason_code"}`, http.StatusBadRequest)
			case errors.Is(err, ErrNotPending):
				http.Error(w, `{"error":"item not pending"}`, http.StatusConflict)
			default:
				logger.Warn("review decide failed", zap.Error(err))
				http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			}
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
		body.Actor = resolveActor(r, body.Actor)
		if body.Action == "" {
			http.Error(w, `{"error":"action required"}`, http.StatusBadRequest)
			return
		}
		if body.Actor == "" {
			http.Error(w, `{"error":"no actor; enable OIDC or pass body.actor in dev"}`, http.StatusForbidden)
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
