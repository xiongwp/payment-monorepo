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

	"github.com/redis/go-redis/v9"

	"reconcile-system/internal/backfill"
	"reconcile-system/internal/cdc"
	"reconcile-system/internal/diffstate"
	"reconcile-system/internal/dsl"
	"reconcile-system/internal/graph"
	"reconcile-system/internal/notifier"
	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
)

// Server admin web HTTP 入口。
type Server struct {
	loader     *script.Loader
	scriptDB   *script.Store
	searcher   *store.Searcher
	cdcMgr     *cdc.Manager
	logger     *zap.Logger
	diffStore  *diffstate.Store     // optional：处置工作流端点用
	dlq        *notifier.RedisDLQ    // optional：DLQ 端点用
	dispatcher *notifier.Dispatcher  // optional：DLQ replay 用
	publisher  *cdc.Publisher        // optional：backfill 端点用
	rdb        redis.UniversalClient // optional：CDC status / SSE 用
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

// WithDiffStore 注入处置工作流的 store；返 self 链式调用。caller 不调 = 端点 503。
func (s *Server) WithDiffStore(d *diffstate.Store) *Server {
	s.diffStore = d
	return s
}

// WithDLQ 注入 DLQ + dispatcher（replay 时要用）；caller 不调 = DLQ 端点 503。
func (s *Server) WithDLQ(q *notifier.RedisDLQ, dispatcher *notifier.Dispatcher) *Server {
	s.dlq = q
	s.dispatcher = dispatcher
	return s
}

// WithPublisher 注 publisher；backfill 端点要用它把回灌行写 Redis。
func (s *Server) WithPublisher(p *cdc.Publisher) *Server {
	s.publisher = p
	return s
}

// WithRedis 注 redis client；CDC status 端点 + SSE 用。
func (s *Server) WithRedis(r redis.UniversalClient) *Server {
	s.rdb = r
	return s
}

// Mount 注册路由到调用方提供的 mux。
//
// 注意：这是 admin 入口，调用方应在外层包一道 auth middleware（同
// config-center 的 IntrospectToken 模式；本包不直接依赖 user-merchant-core）。
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/scripts", s.scriptsRoot)
	mux.HandleFunc("/api/v1/scripts/", s.scriptsByID)
	mux.HandleFunc("/api/v1/scripts/_dry_run", s.scriptDryRun)
	mux.HandleFunc("/api/v1/script/symbols", s.scriptSymbols)
	mux.HandleFunc("/api/v1/search", s.search)
	mux.HandleFunc("/api/v1/meta/tables", s.metaTables)
	mux.HandleFunc("/api/v1/meta/schema/", s.metaSchema)
	mux.HandleFunc("/api/v1/meta/idx_keys", s.metaIdxKeys)
	mux.HandleFunc("/api/v1/cdc/status", s.cdcStatus)
	// 处置工作流 / DLQ / batch trigger（WithDiffStore / WithDLQ 注入后才生效）
	mux.HandleFunc("/api/v1/diffs", s.diffsList)
	mux.HandleFunc("/api/v1/diffs/", s.diffsByID)
	mux.HandleFunc("/api/v1/admin/dlq", s.dlqList)
	mux.HandleFunc("/api/v1/admin/dlq/", s.dlqByID)
	mux.HandleFunc("/api/v1/scripts/_batch_run", s.scriptBatchRun)
	// Graph view（跨服务事件关联图）
	mux.HandleFunc("/api/v1/graph", s.graphView)
	// DSL 模板（运营选模板填参生成 Starlark）
	mux.HandleFunc("/api/v1/dsl/templates", s.dslTemplates)
	mux.HandleFunc("/api/v1/dsl/render", s.dslRender)
	// Backfill / dashboard / SSE
	mux.HandleFunc("/api/v1/admin/backfill", s.adminBackfill)
	mux.HandleFunc("/api/v1/diffs/_stats", s.diffStats)
	mux.HandleFunc("/api/v1/events/stream", s.eventsStream)
	mux.HandleFunc("/admin/", s.editorHTML)
	mux.HandleFunc("/admin", s.editorHTML)
}

// adminBackfill POST /api/v1/admin/backfill
//
// Body: {service, dsn, table, pk_col, index_cols, ignore_cols, batch_size,
//        start_pk, stop_pk, where, where_args}
//
// 同步执行（不返回直到 backfill 完成）。生产可能扫几十万行，5h 上限。
// 调用方建议从 admin web "后台任务" 标签触发，不在 normal request loop。
func (s *Server) adminBackfill(w http.ResponseWriter, r *http.Request) {
	if s.publisher == nil {
		http.Error(w, "publisher not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var spec backfill.Spec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if spec.Service == "" || spec.DSN == "" || spec.Table == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("service/dsn/table required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Hour)
	defer cancel()
	res := backfill.Run(ctx, &spec, s.publisher, s.logger)
	writeJSON(w, http.StatusOK, res)
}

// diffStats GET /api/v1/diffs/_stats — dashboard 数据汇总
func (s *Server) diffStats(w http.ResponseWriter, r *http.Request) {
	if s.diffStore == nil {
		http.Error(w, "diff store not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 5000
	if v := r.URL.Query().Get("sample"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	stats, err := s.diffStore.Stats(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// eventsStream GET /api/v1/events/stream — SSE 实时事件流
func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
	if s.rdb == nil {
		http.Error(w, "redis not configured", http.StatusServiceUnavailable)
		return
	}
	hub := NewSSEHub(s.rdb, s.logger)
	hub.Handle(w, r)
}

// graphView GET /api/v1/graph?index=pi_id&value=pi_xxx[&depth=3&max_nodes=200]
//
// 从 (idx_name, value) 出发 BFS 拉所有关联事件 + 边，admin web 用
// cytoscape.js 画跨服务 DAG。运营查"这笔 PI 链路全貌"用。
func (s *Server) graphView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	idxName := q.Get("index")
	value := q.Get("value")
	if idxName == "" || value == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("index and value required"))
		return
	}
	depth := 3
	if v := q.Get("depth"); v != "" {
		fmt.Sscanf(v, "%d", &depth)
	}
	maxNodes := 200
	if v := q.Get("max_nodes"); v != "" {
		fmt.Sscanf(v, "%d", &maxNodes)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := graph.New(s.searcher).WithLimits(depth, maxNodes).Build(ctx, idxName, value)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.logger.Info("graph view built",
		zap.String("seed_idx", idxName), zap.String("seed_val", value),
		zap.Int("nodes", res.Stats.Nodes), zap.Int("edges", res.Stats.Edges),
		zap.Bool("truncated", res.Stats.Truncated))
	writeJSON(w, http.StatusOK, res)
}

// dslTemplates GET /api/v1/dsl/templates  — 列内置模板（admin web 模板选择器）
func (s *Server) dslTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": dsl.All()})
}

// dslRender POST /api/v1/dsl/render  — 模板 + 参数 → Starlark 代码
//
// Body: {template_id: "amount_match", params: {idx_name: "pi_id", ...}}
//
// 返：{code: "def check(ctx): ..."}
//
// admin web 拿到 code 后塞进 Monaco editor，用户预览 / 微调 / 直接保存。
// 保存走标准 /scripts CRUD 流程。
func (s *Server) dslRender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		TemplateID string            `json:"template_id"`
		Params     map[string]string `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	code, err := dsl.Render(body.TemplateID, body.Params)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":        code,
		"template_id": body.TemplateID,
	})
}

// ─── /api/v1/diffs[/...] 处置工作流 ─────────────────────────────────────

// diffsList GET /api/v1/diffs?state=open[&limit=100]
//
// 列出指定状态的 diff（admin web 值班看 "open 待办"）。
func (s *Server) diffsList(w http.ResponseWriter, r *http.Request) {
	if s.diffStore == nil {
		http.Error(w, "diff store not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state := diffstate.State(r.URL.Query().Get("state"))
	if state == "" {
		state = diffstate.StateOpen
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	out, err := s.diffStore.ListByState(r.Context(), state, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"diffs": out, "state": state})
}

// diffsByID 处理 /api/v1/diffs/{id} / {id}/transition / {id}/audit
func (s *Server) diffsByID(w http.ResponseWriter, r *http.Request) {
	if s.diffStore == nil {
		http.Error(w, "diff store not configured", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/diffs/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		d, err := s.diffStore.Get(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	case action == "transition" && r.Method == http.MethodPost:
		var body struct {
			To   string `json:"to"`
			Note string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		actor := actorOrUnknown(r)
		if err := s.diffStore.Transition(r.Context(), id, diffstate.State(body.To), actor, body.Note); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		// 取最新状态返
		d, _ := s.diffStore.Get(r.Context(), id)
		writeJSON(w, http.StatusOK, d)
	case action == "audit" && r.Method == http.MethodGet:
		log, err := s.diffStore.Audit(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"audit": log})
	case action == "audit/chain" && r.Method == http.MethodGet:
		// 完整链（含 chain_hash），合规审计员看
		log, err := s.diffStore.AuditWithChain(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"audit": log})
	case action == "audit/verify" && r.Method == http.MethodGet:
		// 跑一遍 hash 链校验，合规月度审计 / 异常告警时调
		ok, badIdx, err := s.diffStore.VerifyChain(r.Context(), id)
		resp := map[string]any{"ok": ok, "diff_id": id}
		if err != nil {
			resp["error"] = err.Error()
		}
		if !ok {
			resp["bad_index"] = badIdx
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// ─── /api/v1/admin/dlq[/...] 失败投递重试 ───────────────────────────────

// dlqList GET /api/v1/admin/dlq[?limit=100]
func (s *Server) dlqList(w http.ResponseWriter, r *http.Request) {
	if s.dlq == nil {
		http.Error(w, "dlq not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	entries, err := s.dlq.List(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stats, _ := s.dlq.Stats(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"stats":   stats,
	})
}

// dlqByID 处理 /api/v1/admin/dlq/{id}/replay
func (s *Server) dlqByID(w http.ResponseWriter, r *http.Request) {
	if s.dlq == nil || s.dispatcher == nil {
		http.Error(w, "dlq not configured", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/dlq/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if action != "replay" || r.Method != http.MethodPost {
		http.Error(w, "expected POST /api/v1/admin/dlq/{id}/replay", http.StatusNotFound)
		return
	}
	if err := s.dlq.Replay(r.Context(), s.dispatcher, id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// ─── /api/v1/scripts/_batch_run 批处理对账 ──────────────────────────────

// scriptBatchRun POST /api/v1/scripts/_batch_run
//
// Body: { script_id: string, params: map[string]string }
//
// 与 /scripts/<id>/run 区别：params 自动塞 batch_mode=true，脚本可识别走批处理
// 分支（典型：scan_index 一次拉更多 + 分页处理 / 加 since/until 时间过滤）。
//
// 实现复用 Loader.Run；脚本作者负责自己的批处理语义（reconplatform 不规定
// 怎么 batch — 业务对账逻辑差异太大，用 ctx.params 注入参数最灵活）。
func (s *Server) scriptBatchRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ScriptID string            `json:"script_id"`
		Params   map[string]string `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.ScriptID == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("script_id required"))
		return
	}
	if body.Params == nil {
		body.Params = map[string]string{}
	}
	body.Params["batch_mode"] = "true"
	// 30min 上限：批处理可能扫几十万行
	runCtx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	sctx := script.NewContext(runCtx, s.searcher, s.zapAdapter(), body.Params)
	res := s.loader.Run(sctx, body.ScriptID, "batch:"+actorOrUnknown(r))
	if err := s.scriptDB.SaveResult(r.Context(), res); err != nil {
		s.logger.Warn("save batch result failed", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, res)
}

// scriptDryRun POST /api/v1/scripts/_dry_run
//
// Body: { code: string, params?: map[string]string }
//
// 编译 + 执行 code（不保存到 Redis、不写历史结果），同步返 Result。
//
// 用途：admin web 编辑器 "Dry Run" 按钮 — 运营写完脚本，先试跑
// 看 diff 输出是否符合预期，再决定保存 + 上线。
//
// 安全：5 分钟硬超时；脚本受 Starlark MaxExecutionSteps 上限保护；
// dry-run 调用 ctx.scan_index / get_by_index 走真 Redis（读取只读，
// 不会写到业务侧），可选未来加 sample_only flag 限定扫描行数。
func (s *Server) scriptDryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Code   string            `json:"code"`
		Params map[string]string `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("empty code"))
		return
	}
	runCtx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if body.Params == nil {
		body.Params = map[string]string{}
	}
	body.Params["dry_run"] = "true"
	sctx := script.NewContext(runCtx, s.searcher, s.zapAdapter(), body.Params)
	res := s.loader.RunCode(sctx, "_dryrun", body.Code, "dry_run:"+actorOrUnknown(r))
	writeJSON(w, http.StatusOK, res)
}

// scriptSymbols GET /api/v1/script/symbols
//
// 返完整的 Starlark 脚本环境符号清单：
//
//   - modules：所有 builtin / 动态注册的 host 包（脚本里 load("@<name>") 引入）
//   - ctx：def check(ctx) 里 ctx 对象上可用的属性 / 方法
//   - event：单个 event 对象的属性 / 方法
//   - event_list：events.find() 返回的列表对象的属性 / 方法
//
// admin web 的 Monaco completionItemProvider 调本端点拿这份 schema，
// 写脚本时 ctx. / event. / load("@") 处自动弹出补齐。
//
// 实现：每次请求重新 collect — engine 是只增不减的，反射开销小（典型 < 1ms）。
// 如果 RegisterModule 频繁可加 cache，按需。
func (s *Server) scriptSymbols(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	syms := script.CollectSymbols(s.loader.Engine())
	writeJSON(w, http.StatusOK, syms)
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
		// ValidateWithLint 同时返编译错 + lint issues。
		// admin web 编辑器侧栏 marker 用 issues；ok 标志只看编译错。
		compileErr, issues := s.loader.ValidateWithLint(body.Code)
		resp := map[string]any{"ok": compileErr == nil, "issues": issues}
		if compileErr != nil {
			resp["error"] = compileErr.Error()
		}
		writeJSON(w, http.StatusOK, resp)

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

	start := time.Now()
	if value != "" {
		// 精确查询：返关联事件
		events, err := s.searcher.SearchByIndex(ctx, idxName, value)
		dur := time.Since(start)
		if err != nil {
			s.logger.Warn("search failed",
				zap.String("index", idxName), zap.String("value", value),
				zap.Duration("dur", dur), zap.Error(err))
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		// 日志：方便 oncall 看 "实时搜索为什么搜不到":
		//   hits=0 + CDC 没流入 → CDC 没连上 / 表不在订阅清单
		//   hits=0 + 该 idx 有别的 value → 输入的具体 value 真没事件
		s.logger.Info("search by index: exact",
			zap.String("index", idxName), zap.String("value", value),
			zap.Int("hits", len(events)), zap.Duration("dur", dur))
		writeJSON(w, http.StatusOK, map[string]any{
			"index":  idxName,
			"value":  value,
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
	dur := time.Since(start)
	if err != nil {
		s.logger.Warn("search scan failed",
			zap.String("index", idxName), zap.String("prefix", prefix),
			zap.Duration("dur", dur), zap.Error(err))
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.logger.Info("search by index: scan",
		zap.String("index", idxName), zap.String("prefix", prefix),
		zap.Int("hits", len(values)), zap.Duration("dur", dur))
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
