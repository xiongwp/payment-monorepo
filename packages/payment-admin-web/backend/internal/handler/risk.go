// handler/risk.go —— 风控运营平台 API。代理到 risk-manage 的 admin HTTP 端点
// （review queue / outcome feedback / decision audit），由 BFF 做：
//   1. 鉴权统一（admin BFF 已有 ADMIN_BEARER_TOKEN，前端不直连 risk-manage）
//   2. 给前端"扁平化"的列表 / 详情 / 决议接口
//   3. 跨服务字段聚合（如 review item 关联到 PI 的金额展示）— 暂未做，留扩展
//
// risk-manage 端点：
//   GET  /admin/review/list?status=&limit=&offset=
//   GET  /admin/review/get?id=
//   POST /admin/review/decide  Body: {id, action, actor, reason}
//   GET  /admin/feedback/recent?limit=
//   GET  /admin/feedback/get?id=
//   POST /admin/feedback/outcome  Body: {decision_id, source, is_fraud, actor, notes}
//   GET  /admin/audit/decisions?limit=
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/mux"
)

func pathVar(r *http.Request, key string) string { return mux.Vars(r)[key] }

type RiskHandler struct {
	baseURL string // 默认 http://risk-manage:9590
	token   string // optional：risk-manage 自己也开了 admin auth 时填这里
	client  *http.Client
}

func NewRiskHandler() *RiskHandler {
	base := os.Getenv("RISK_ADMIN_HTTP_ADDR")
	if base == "" {
		base = "http://risk-manage:9590"
	}
	return &RiskHandler{
		baseURL: base,
		token:   os.Getenv("RISK_ADMIN_TOKEN"),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

// gracefulDownstream 下游不可用时统一返友好降级响应。
// 所有 risk-manage proxy 端点都用这个 wrapper —— admin web 不再因为某个
// 下游不可用就弹红错；改去 Config Center 是"动态配置"的官方入口。
func gracefulDownstream(w http.ResponseWriter, key string, err error) {
	hint := "risk-manage 下游不可用；规则 / 阈值类配置请在 Config Center " +
		"namespace=risk-manage 或 reconplatform 管理"
	gracefulDownstreamHint(w, key, err, hint)
}

// gracefulDownstreamHint 同 gracefulDownstream，但允许定制 hint 文案，
// 给 kms / order / payment-channel 等其他下游 handler 复用。
//
// 响应体：
//
//	{ <key>: [], total: 0, service_status: "unavailable",
//	  service_error: "<grpc/http err>", hint: "<人类可读的引导>" }
//
// 前端按 service_status 字段决定是否展示降级 banner（不弹红错）。
func gracefulDownstreamHint(w http.ResponseWriter, key string, err error, hint string) {
	writeJSON(w, map[string]any{
		key:              []any{},
		"total":          0,
		"service_status": "unavailable",
		"service_error":  errString(err),
		"hint":           hint,
	})
}

// errString nil-safe err.Error()，避免 nil deref。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ── Reviews ──────────────────────────────────────────────────────

// GET /api/risk/reviews?status=pending&limit=100&offset=0
//
// risk-manage 的 /admin/review/* 端点是 v2 路线图项（review queue 真实数据未
// 写完），下游 connection refused / 404 不该让前端崩。降级为友好空响应 +
// service_status 字段，前端弹横幅。
func (h *RiskHandler) ListReviews(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	if v := r.URL.Query().Get("status"); v != "" {
		q.Set("status", v)
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		q.Set("limit", v)
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		q.Set("offset", v)
	}
	body, err := h.proxyGet(r.Context(), "/admin/review/list?"+q.Encode())
	if err != nil {
		// 下游不可用 → 返空列表 + 状态信息（前端不报错）
		writeJSON(w, map[string]any{
			"items":          []any{},
			"total":          0,
			"service_status": "unavailable",
			"service_error":  err.Error(),
			"hint":           "review queue 模块暂未实装；规则配置请用 Config Center namespace=risk-manage 管理",
		})
		return
	}
	var items []map[string]any
	_ = json.Unmarshal(body, &items)
	writeJSON(w, map[string]any{"items": items, "total": len(items), "service_status": "ok"})
}

// GET /api/risk/reviews/{id}
func (h *RiskHandler) GetReview(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		// gorilla mux path var
		id = pathVar(r, "id")
	}
	if id == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	body, err := h.proxyGet(r.Context(), "/admin/review/get?id="+url.QueryEscape(id))
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var item map[string]any
	_ = json.Unmarshal(body, &item)
	writeJSON(w, item)
}

// POST /api/risk/reviews/decide  Body: {id, action, actor, reason}
func (h *RiskHandler) DecideReview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Actor  string `json:"actor"`
		Reason string `json:"reason"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.ID == "" || in.Action == "" || in.Actor == "" {
		writeError(w, http.StatusBadRequest, "id+action+actor required")
		return
	}
	body, err := h.proxyPost(r.Context(), "/admin/review/decide", in)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/dashboard/summary  → 一次拿 rule_count / 队列深度 / 模型 /
// 决策分布。代理到 risk-manage /admin/dashboard/summary。
func (h *RiskHandler) DashboardSummary(w http.ResponseWriter, r *http.Request) {
	body, err := h.proxyGet(r.Context(), "/admin/dashboard/summary")
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// POST /api/risk/rules/mode  Body: {id, shadow}
// 切换规则 enforce ↔ shadow（不重启）。给"规则预测试"工作流用。
func (h *RiskHandler) RuleSetMode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Shadow bool   `json:"shadow"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.ID == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	body, err := h.proxyPost(r.Context(), "/admin/rules/mode", in)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/rules/list → 当前在跑的规则集（id+name+type+config_json+
// mode+weight+rollout+enabled）。规则编辑 UI 渲染表格用。
func (h *RiskHandler) RulesList(w http.ResponseWriter, r *http.Request) {
	body, err := h.proxyGet(r.Context(), "/admin/rules/list")
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out []map[string]any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, map[string]any{"items": out})
}

// POST /api/risk/rules/update Body: RuleDef
// 单条规则原子热更新。risk-manage 端会跑 schema 校验；BFF 透传 400 / 500。
func (h *RiskHandler) RulesUpdate(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if _, ok := in["id"].(string); !ok {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	if _, ok := in["type"].(string); !ok {
		writeError(w, http.StatusBadRequest, "type required")
		return
	}
	body, status, err := h.proxyPostStatus(r.Context(), "/admin/rules/update", in)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	writeProxyResult(w, body, status)
}

// POST /api/risk/rules/simulate Body: RuleDef (?sample=N optional)
//
// 给规则编辑器"试运行"按钮用：候选规则跑过 schema 校验 + 重放最近 N 条
// 决策，返回 {sample, hits, would_newly_block, would_keep_block,
// estimated_precision, notes[]}。失败 → 透传 400 + body.error 给 UI。
func (h *RiskHandler) RulesSimulate(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if _, ok := in["type"].(string); !ok {
		writeError(w, http.StatusBadRequest, "type required")
		return
	}
	path := "/admin/rules/simulate"
	if v := r.URL.Query().Get("sample"); v != "" {
		path += "?sample=" + url.QueryEscape(v)
	}
	body, status, err := h.proxyPostStatus(r.Context(), path, in)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	writeProxyResult(w, body, status)
}

// POST /api/risk/rules/delete Body: {id, reason}
func (h *RiskHandler) RulesDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.ID == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	body, status, err := h.proxyPostStatus(r.Context(), "/admin/rules/delete", in)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	writeProxyResult(w, body, status)
}

// POST /api/risk/reviews/decide-bulk Body: {ids[], action, actor, reason}
// → 批量审核（卡测试攻击下一次性 reject 50+ 笔同模式）
func (h *RiskHandler) DecideReviewBulk(w http.ResponseWriter, r *http.Request) {
	h.proxyPostThrough(w, r, "/admin/review/decide-bulk")
}

// POST /api/risk/extsignal/push Body: {provider, entity_type, entity_key, score, reasons, raw_json}
// → 外部反欺诈系统推一条分数到 risk-manage 缓存
func (h *RiskHandler) ExtSignalPush(w http.ResponseWriter, r *http.Request) {
	h.proxyPostThrough(w, r, "/admin/extsignal/push")
}

// GET /api/risk/extsignal/get?provider=&type=&key= → 查缓存
func (h *RiskHandler) ExtSignalGet(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	for _, k := range []string{"provider", "type", "key"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	body, err := h.proxyGet(r.Context(), "/admin/extsignal/get?"+q.Encode())
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/extsignal/stats → 缓存大小
func (h *RiskHandler) ExtSignalStats(w http.ResponseWriter, r *http.Request) {
	body, err := h.proxyGet(r.Context(), "/admin/extsignal/stats")
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/whoami → 当前 admin token role + key_id (给前端 RBAC UI)
func (h *RiskHandler) Whoami(w http.ResponseWriter, r *http.Request) {
	body, err := h.proxyGet(r.Context(), "/admin/whoami")
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// POST /api/risk/audit/chain/verify Body: {limit, start_prev}
// → 跑 audit chain 完整性验证，给合规 / 内审用
func (h *RiskHandler) AuditChainVerify(w http.ResponseWriter, r *http.Request) {
	h.proxyPostThrough(w, r, "/admin/audit/chain/verify")
}

// GET /api/risk/decisions/search → 按 merchant_id / customer_id / ip / verdict / 时间过滤
func (h *RiskHandler) DecisionSearch(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	for _, k := range []string{"merchant_id", "customer_id", "ip", "verdict", "since", "until", "limit"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	body, err := h.proxyGet(r.Context(), "/admin/audit/search?"+q.Encode())
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// POST /api/risk/decisions/explain Body: {decision_id}
// → 把历史决策用当前规则集 re-evaluate，返回原决策 vs 重放对比 + 输入快照。
// 给运营调试客户投诉 / 误伤分析用。
func (h *RiskHandler) DecisionExplain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DecisionID string `json:"decision_id"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.DecisionID == "" {
		writeError(w, http.StatusBadRequest, "decision_id required")
		return
	}
	body, status, err := h.proxyPostStatus(r.Context(), "/admin/explain", in)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	writeProxyResult(w, body, status)
}

// GET /api/risk/rules/export → text/yaml 当前规则集（直接 paste 跨环境推送）
func (h *RiskHandler) RulesExport(w http.ResponseWriter, r *http.Request) {
	body, err := h.proxyGet(r.Context(), "/admin/rules/export")
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="risk-rules.yaml"`)
	_, _ = w.Write(body)
}

// POST /api/risk/rules/import?dry_run=true Body: YAML
// → 验证 + 返回 diff (dry_run) 或真正落地 (dry_run=false)
func (h *RiskHandler) RulesImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	dryRun := "true"
	if v := r.URL.Query().Get("dry_run"); v == "false" {
		dryRun = "false"
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		h.baseURL+"/admin/rules/import?dry_run="+dryRun, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "text/yaml")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	writeProxyResult(w, out, resp.StatusCode)
}

// GET /api/risk/mlscore/challengers → 列出 champion + 所有 challenger
func (h *RiskHandler) ChallengersList(w http.ResponseWriter, r *http.Request) {
	body, err := h.proxyGet(r.Context(), "/admin/mlscore/challengers")
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// POST /api/risk/mlscore/challengers/register Body: {name, model_ver, ...}
func (h *RiskHandler) ChallengerRegister(w http.ResponseWriter, r *http.Request) {
	h.proxyPostThrough(w, r, "/admin/mlscore/challengers/register")
}

// POST /api/risk/mlscore/challengers/promote Body: {name}
func (h *RiskHandler) ChallengerPromote(w http.ResponseWriter, r *http.Request) {
	h.proxyPostThrough(w, r, "/admin/mlscore/challengers/promote")
}

// POST /api/risk/mlscore/challengers/drop Body: {name}
func (h *RiskHandler) ChallengerDrop(w http.ResponseWriter, r *http.Request) {
	h.proxyPostThrough(w, r, "/admin/mlscore/challengers/drop")
}

// proxyPostThrough 通用 POST 代理：直接转发 body，透传 status code +
// upstream JSON body 给前端（让 risk-manage 的错误信息直达 UI）。
func (h *RiskHandler) proxyPostThrough(w http.ResponseWriter, r *http.Request, path string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<14))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	out, status, err := h.proxyPostStatus(r.Context(), path, json.RawMessage(body))
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	writeProxyResult(w, out, status)
}

// GET / POST /api/risk/mlscore/override → 运营手动 ML 降级开关
func (h *RiskHandler) MLScoreOverrideGet(w http.ResponseWriter, r *http.Request) {
	body, err := h.proxyGet(r.Context(), "/admin/mlscore/override")
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

func (h *RiskHandler) MLScoreOverrideSet(w http.ResponseWriter, r *http.Request) {
	h.proxyPostThrough(w, r, "/admin/mlscore/override")
}

func (h *RiskHandler) MLScoreOverrideClear(w http.ResponseWriter, r *http.Request) {
	h.proxyPostThrough(w, r, "/admin/mlscore/override/clear")
}

// GET /api/risk/mlscore/abtest?min_labeled=N&bootstrap=N
// → champion-challenger 显著性检验报告。
func (h *RiskHandler) MLScoreABTest(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	for _, k := range []string{"min_labeled", "bootstrap"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	body, err := h.proxyGet(r.Context(), "/admin/mlscore/abtest?"+q.Encode())
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/dashboard/cohort/timeseries?group_by=&key=&bucket=hour|day|week
// → 时间序列 cohort：给指定 cohort（或空 key=全平台）画 block_rate /
// fraud_rate 随时间的趋势图。
func (h *RiskHandler) DashboardCohortTimeseries(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	for _, k := range []string{"group_by", "key", "bucket"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	body, err := h.proxyGet(r.Context(), "/admin/dashboard/cohort/timeseries?"+q.Encode())
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/dashboard/cohort?group_by=merchant_id|country|payment_method&min_total=N
// → 按运营维度拆分决策 + outcome 的统计快照。
func (h *RiskHandler) DashboardCohort(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	if v := r.URL.Query().Get("group_by"); v != "" {
		q.Set("group_by", v)
	}
	if v := r.URL.Query().Get("min_total"); v != "" {
		q.Set("min_total", v)
	}
	body, err := h.proxyGet(r.Context(), "/admin/dashboard/cohort?"+q.Encode())
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/rules/insights → 每条规则的运营 KPI（last_hit / precision /
// ROI / silent flag），给运营按 ROI 排序看哪些规则该留 / 改 / 删。
func (h *RiskHandler) RulesInsights(w http.ResponseWriter, r *http.Request) {
	body, err := h.proxyGet(r.Context(), "/admin/rules/insights")
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/rules/overlap?min_both=N → 规则两两 co-fire 矩阵（找冗余 + 冲突）
func (h *RiskHandler) RulesOverlap(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	if v := r.URL.Query().Get("min_both"); v != "" {
		q.Set("min_both", v)
	}
	body, err := h.proxyGet(r.Context(), "/admin/rules/overlap?"+q.Encode())
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/rules/audit?rule_id=X&limit=100
func (h *RiskHandler) RulesAudit(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	if v := r.URL.Query().Get("rule_id"); v != "" {
		q.Set("rule_id", v)
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		q.Set("limit", v)
	}
	body, err := h.proxyGet(r.Context(), "/admin/rules/audit?"+q.Encode())
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out []map[string]any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, map[string]any{"items": out})
}

// GET /api/risk/dashboard/recall → 召回 / 准确率 / FPR
func (h *RiskHandler) DashboardRecall(w http.ResponseWriter, r *http.Request) {
	body, err := h.proxyGet(r.Context(), "/admin/dashboard/recall")
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// ── Case workbench (claim / release / escalate / note / queues) ─────────────

type caseMutationBody struct {
	ID     string `json:"id"`
	Actor  string `json:"actor"`
	Reason string `json:"reason,omitempty"`
	Body   string `json:"body,omitempty"`
}

// POST /api/risk/reviews/claim    Body: {id, actor}
func (h *RiskHandler) ClaimReview(w http.ResponseWriter, r *http.Request) {
	h.caseMutation(w, r, "/admin/review/claim", "claim")
}

// POST /api/risk/reviews/release  Body: {id, actor}
func (h *RiskHandler) ReleaseReview(w http.ResponseWriter, r *http.Request) {
	h.caseMutation(w, r, "/admin/review/release", "release")
}

// POST /api/risk/reviews/escalate Body: {id, actor, reason}
func (h *RiskHandler) EscalateReview(w http.ResponseWriter, r *http.Request) {
	h.caseMutation(w, r, "/admin/review/escalate", "escalate")
}

// POST /api/risk/reviews/note     Body: {id, actor, body}
func (h *RiskHandler) AddReviewNote(w http.ResponseWriter, r *http.Request) {
	h.caseMutation(w, r, "/admin/review/note", "note")
}

func (h *RiskHandler) caseMutation(w http.ResponseWriter, r *http.Request, path, op string) {
	var in caseMutationBody
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.ID == "" || in.Actor == "" {
		writeError(w, http.StatusBadRequest, "id+actor required")
		return
	}
	if op == "note" && in.Body == "" {
		writeError(w, http.StatusBadRequest, "body required")
		return
	}
	body, err := h.proxyPost(r.Context(), path, in)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/reviews/by-assignee?actor=X&status=in_review&limit=100
func (h *RiskHandler) ReviewsByAssignee(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	q.Set("actor", r.URL.Query().Get("actor"))
	if s := r.URL.Query().Get("status"); s != "" {
		q.Set("status", s)
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		q.Set("limit", l)
	}
	body, err := h.proxyGet(r.Context(), "/admin/review/by-assignee?"+q.Encode())
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// GET /api/risk/reviews/overdue?limit=100
func (h *RiskHandler) ReviewsOverdue(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	if l := r.URL.Query().Get("limit"); l != "" {
		q.Set("limit", l)
	}
	path := "/admin/review/overdue"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	body, err := h.proxyGet(r.Context(), path)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// ── Feedback / Outcomes ──────────────────────────────────────────

// GET /api/risk/outcomes/recent?limit=100
func (h *RiskHandler) RecentOutcomes(w http.ResponseWriter, r *http.Request) {
	limit := r.URL.Query().Get("limit")
	if limit == "" {
		limit = "100"
	}
	body, err := h.proxyGet(r.Context(), "/admin/feedback/recent?limit="+limit)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var items []map[string]any
	_ = json.Unmarshal(body, &items)
	writeJSON(w, map[string]any{"items": items, "total": len(items)})
}

// GET /api/risk/outcomes/{decision_id}
func (h *RiskHandler) GetOutcomes(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	if id == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	body, err := h.proxyGet(r.Context(), "/admin/feedback/get?id="+url.QueryEscape(id))
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var items []map[string]any
	_ = json.Unmarshal(body, &items)
	writeJSON(w, items)
}

// POST /api/risk/outcomes  Body: {decision_id, source, is_fraud, actor, notes}
func (h *RiskHandler) RecordOutcome(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DecisionID string `json:"decision_id"`
		Source     string `json:"source"`
		IsFraud    bool   `json:"is_fraud"`
		Actor      string `json:"actor"`
		Notes      string `json:"notes"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.DecisionID == "" || in.Source == "" {
		writeError(w, http.StatusBadRequest, "decision_id+source required")
		return
	}
	body, err := h.proxyPost(r.Context(), "/admin/feedback/outcome", in)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	writeJSON(w, out)
}

// ── Audit / Decisions ────────────────────────────────────────────

// GET /api/risk/decisions?limit=100
func (h *RiskHandler) ListDecisions(w http.ResponseWriter, r *http.Request) {
	limit := r.URL.Query().Get("limit")
	if limit == "" {
		limit = "100"
	}
	if n, err := strconv.Atoi(limit); err == nil && n > 1000 {
		limit = "1000"
	}
	body, err := h.proxyGet(r.Context(), "/admin/audit/decisions?limit="+limit)
	if err != nil {
		gracefulDownstream(w, "items", err)
		return
	}
	var rows []map[string]any
	_ = json.Unmarshal(body, &rows)
	writeJSON(w, map[string]any{"items": rows, "total": len(rows), "service_status": "ok"})
}

// ── HTTP proxy helpers ───────────────────────────────────────────

func (h *RiskHandler) proxyGet(ctx context.Context, path string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+path, nil)
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &proxyErr{Status: resp.StatusCode, Body: string(body)}
	}
	return body, nil
}

func (h *RiskHandler) proxyPost(ctx context.Context, path string, payload any) ([]byte, error) {
	js, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+path, bytes.NewReader(js))
	req.Header.Set("Content-Type", "application/json")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &proxyErr{Status: resp.StatusCode, Body: string(body)}
	}
	return body, nil
}

type proxyErr struct {
	Status int
	Body   string
}

func (e *proxyErr) Error() string {
	return "risk admin upstream HTTP " + strconv.Itoa(e.Status) + ": " + e.Body
}

// proxyPostStatus 跟 proxyPost 一样转发，但保留 upstream 的 status code +
// raw body 透传给 caller。用于 admin 编辑 UI 想看到 risk-manage 的 400 校验
// 错误（"unknown rule type" / "config invalid: ..."）这类细节。
func (h *RiskHandler) proxyPostStatus(ctx context.Context, path string, payload any) ([]byte, int, error) {
	js, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+path, bytes.NewReader(js))
	req.Header.Set("Content-Type", "application/json")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}
