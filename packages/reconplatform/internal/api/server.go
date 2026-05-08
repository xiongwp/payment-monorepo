// Package api - reconplatform admin web 的 HTTP server。
//
// 端点（全部 JSON）：
//
//   POST /api/v1/scripts              新建脚本
//   GET  /api/v1/scripts              列表（按 updated_at desc）
//   GET  /api/v1/scripts/:id          单条详情（含 code）
//   PUT  /api/v1/scripts/:id          更新（保存）
//   DELETE /api/v1/scripts/:id        删除
//   POST /api/v1/scripts/:id/run      立即运行（同步返结果）
//   POST /api/v1/scripts/:id/validate 仅语法检查（不保存不运行）
//   GET  /api/v1/scripts/:id/results  历史运行结果（最近 N 次）
//
//   GET  /api/v1/search?index=&value=&prefix=&limit=
//                                     业务 key 实时搜索（编辑器右侧用）
//
//   GET  /api/v1/meta/tables          所有同步过的表 ["<svc>:<table>", ...]
//   GET  /api/v1/meta/schema/:svc/:table  表的列定义 [{name,type,...}]
//   GET  /api/v1/meta/idx_keys        可用索引列名 ["pi_id", "order_id", ...]
//
//   GET  /api/v1/cdc/status           各 CDC runner 状态（service / lastFile / lastPos）
//
//   GET  /admin/                      HTML 编辑器（embed）
//   GET  /admin/scripts/:id           编辑器（带脚本预加载）
//
//   GET  /healthz / /readyz           健康
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"reconcile-system/internal/cdc"
	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
)

// Server admin web HTTP 入口。
type Server struct {
	loader   *script.Loader
	scriptDB *script.Store
	searcher *store.Searcher
	cdcMgr   *cdc.Manager
	logger   *zap.Logger
}

// New 构造。
func New(loader *script.Loader, scriptDB *script.Store, searcher *store.Searcher, cdcMgr *cdc.Manager, logger *zap.Logger) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Server{
		loader:   loader,
		scriptDB: scriptDB,
		searcher: searcher,
		cdcMgr:   cdcMgr,
		logger:   logger,
	}
}

// Mount 注册路由到调用方提供的 mux。
//
// 注意：这是 admin 入口，调用方应在外层包一道 auth middleware（同
// config-center 的 IntrospectToken 模式；本包不直接依赖 user-merchant-core）。
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/scripts", s.scriptsRoot)
	mux.HandleFunc("/api/v1/scripts/", s.scriptsByID)
	mux.HandleFunc("/api/v1/search", s.search)
	mux.HandleFunc("/api/v1/meta/tables", s.metaTables)
	mux.HandleFunc("/api/v1/meta/schema/", s.metaSchema)
	mux.HandleFunc("/api/v1/meta/idx_keys", s.metaIdxKeys)
	mux.HandleFunc("/api/v1/cdc/status", s.cdcStatus)
	mux.HandleFunc("/admin/", s.editorHTML)
	mux.HandleFunc("/admin", s.editorHTML)
}

// ─── /api/v1/scripts ─────────────────────────────────────────────

// POST 新建 / GET 列表
func (s *Server) scriptsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		defs, err := s.scriptDB.ListDefs(r.Context(), 200)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		// 不返 code，避免 list 太大；详情 GET 才带 code
		out := make([]map[string]any, 0, len(defs))
		for _, d := range defs {
			out = append(out, map[string]any{
				"id":         d.ID,
				"name":       d.Name,
				"schedule":   d.Schedule,
				"triggers":   d.Triggers,
				"updated_at": d.UpdatedAt,
				"updated_by": d.UpdatedBy,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"scripts": out})

	case http.MethodPost:
		var body scriptPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if body.ID == "" {
			body.ID = generateScriptID(body.Name)
		}
		// 先 validate
		if err := s.loader.Validate(body.Code); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("validate: %w", err))
			return
		}
		ver, err := s.scriptDB.SaveDef(r.Context(),
			body.ID, body.Name, body.Code, body.Schedule, body.Triggers, actorOrUnknown(r))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		// 加载到 loader 内存
		def, _ := s.scriptDB.LoadDef(r.Context(), body.ID)
		if err := s.loader.Upsert(body.ID, def); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": body.ID, "version": ver})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// /api/v1/scripts/<id>[/run|/validate|/results]
func (s *Server) scriptsByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/scripts/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		def, err := s.scriptDB.LoadDef(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, def)

	case action == "" && r.Method == http.MethodPut:
		var body scriptPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.loader.Validate(body.Code); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("validate: %w", err))
			return
		}
		ver, err := s.scriptDB.SaveDef(r.Context(),
			id, body.Name, body.Code, body.Schedule, body.Triggers, actorOrUnknown(r))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		def, _ := s.scriptDB.LoadDef(r.Context(), id)
		if err := s.loader.Upsert(id, def); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "version": ver})

	case action == "" && r.Method == http.MethodDelete:
		if err := s.scriptDB.DeleteDef(r.Context(), id, actorOrUnknown(r)); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		s.loader.Remove(id)
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})

	case action == "validate" && r.Method == http.MethodPost:
		var body scriptPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.loader.Validate(body.Code); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case action == "run" && r.Method == http.MethodPost:
		// 同步运行；返完整 Result。
		// 5 分钟硬超时 — 脚本写得离谱也不能挂死 admin web。
		runCtx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		params := map[string]string{}
		if r.URL.RawQuery != "" {
			for k, vs := range r.URL.Query() {
				if len(vs) > 0 {
					params[k] = vs[0]
				}
			}
		}
		sctx := script.NewContext(runCtx, s.searcher, s.zapAdapter(), params)
		res := s.loader.Run(sctx, id, "manual:"+actorOrUnknown(r))
		if err := s.scriptDB.SaveResult(r.Context(), res); err != nil {
			s.logger.Warn("save result failed", zap.Error(err))
		}
		writeJSON(w, http.StatusOK, res)

	case action == "results" && r.Method == http.MethodGet:
		results, err := s.scriptDB.ListResults(r.Context(), id, 50)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})

	case action == "versions" && r.Method == http.MethodGet:
		// 版本历史列表（不带 code，列表展示用）
		versions, err := s.scriptDB.ListVersions(r.Context(), id, 50)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": versions})

	case strings.HasPrefix(action, "versions/") && r.Method == http.MethodGet:
		// 单个版本详情：/scripts/<id>/versions/<N>  — 含 code，给"看历史 / diff"用
		verStr := strings.TrimPrefix(action, "versions/")
		var ver int64
		if _, err := fmt.Sscanf(verStr, "%d", &ver); err != nil || ver <= 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid version: %q", verStr))
			return
		}
		v, err := s.scriptDB.GetVersion(r.Context(), id, ver)
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, v)

	case action == "rollback" && r.Method == http.MethodPost:
		// POST /scripts/<id>/rollback  body: {"version": 5}
		var body struct {
			Version int64 `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if body.Version <= 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("version required and > 0"))
			return
		}
		newVer, err := s.scriptDB.Rollback(r.Context(), id, body.Version, actorOrUnknown(r))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		// 同步重 load 到 loader 内存
		def, _ := s.scriptDB.LoadDef(r.Context(), id)
		if def != nil {
			_ = s.loader.Upsert(id, def)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":           id,
			"rolled_back":  body.Version,
			"new_version":  newVer,
		})

	default:
		http.Error(w, "method/action not supported", http.StatusMethodNotAllowed)
	}
}

// ─── /api/v1/search ─────────────────────────────────────────────

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	idxName := q.Get("index")
	value := q.Get("value")
	prefix := q.Get("prefix")
	if idxName == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("index required"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if value != "" {
		// 精确查询：返关联事件
		events, err := s.searcher.SearchByIndex(ctx, idxName, value)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"index": idxName,
			"value": value,
			"events": events,
		})
		return
	}

	// 模糊补全：列出 prefix* 的 value
	limit := 100
	if v := q.Get("limit"); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		if n > 0 && n <= 500 {
			limit = n
		}
	}
	values, err := s.searcher.ScanIndex(ctx, idxName, prefix, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"index":  idxName,
		"prefix": prefix,
		"values": values,
	})
}

// ─── /api/v1/meta/* ─────────────────────────────────────────────

func (s *Server) metaTables(w http.ResponseWriter, r *http.Request) {
	tables, err := s.searcher.ListSchemas(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": tables})
}

func (s *Server) metaSchema(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/meta/schema/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("path must be /api/v1/meta/schema/<svc>/<table>"))
		return
	}
	jsonStr, err := s.searcher.GetSchema(r.Context(), parts[0], parts[1])
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(jsonStr))
}

func (s *Server) metaIdxKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.searcher.ListIndexKeys(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"index_keys": keys})
}

// ─── /api/v1/cdc/status ─────────────────────────────────────────

func (s *Server) cdcStatus(w http.ResponseWriter, r *http.Request) {
	if s.cdcMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runners": []any{}, "note": "cdc manager not configured"})
		return
	}
	stats := s.cdcMgr.Stats(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"runners": stats})
}

// ─── /admin/ HTML ───────────────────────────────────────────────

func (s *Server) editorHTML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(editorHTMLContent))
}

// ─── helpers ─────────────────────────────────────────────────────

type scriptPayload struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Code     string   `json:"code"`
	Schedule string   `json:"schedule"`
	Triggers []string `json:"triggers"`
}

func generateScriptID(name string) string {
	base := strings.ToLower(strings.TrimSpace(name))
	base = strings.ReplaceAll(base, " ", "_")
	base = strings.ReplaceAll(base, "-", "_")
	if base == "" {
		base = "script"
	}
	return fmt.Sprintf("%s_%d", base, time.Now().UnixMilli())
}

func actorOrUnknown(r *http.Request) string {
	if v := r.Header.Get("X-Admin-User"); v != "" {
		return v
	}
	return "unknown"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

// zapAdapter 把 *zap.Logger 适配成 script.Logger（kv 形式）。
type zapAdapt struct{ z *zap.Logger }

func (s *Server) zapAdapter() script.Logger { return &zapAdapt{z: s.logger} }

func (a *zapAdapt) Info(msg string, kv ...any)  { a.z.Sugar().Infow(msg, kv...) }
func (a *zapAdapt) Warn(msg string, kv ...any)  { a.z.Sugar().Warnw(msg, kv...) }
func (a *zapAdapt) Error(msg string, kv ...any) { a.z.Sugar().Errorw(msg, kv...) }
