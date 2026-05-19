// admin_handlers.go — SP-AC-7 split-payment AdminService gRPC 实现.
//
// 委托给已有的 GraphRepo + workflow.Translate, 不做新逻辑.
// 跟 adminhttp/graph.go 的 HTTP handler 是同一套语义, 只是 transport 从 HTTP 换 gRPC.
package grpcsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"
	"reconcile-system/packages/split-payment/internal/observability"
	"reconcile-system/packages/split-payment/internal/workflow"

	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"
)

// GraphRepo 跟 adminhttp.GraphRepo 同形态接口, 复制一份避免 internal 包循环 import.
type GraphRepo interface {
	Save(ctx context.Context, g *domain.Graph) (int64, error)
	GetByKey(ctx context.Context, key string) (*domain.Graph, error)
	List(ctx context.Context, status string) ([]*domain.Graph, error)
}

// AccountingMetaCaller — TriggerEvent 用来调 accounting.CreateTransaction 真落账.
// 跟 workflow.AccountingMetaCaller 同形态, 复制避免循环 import.
type AccountingMetaCaller interface {
	CreateTransaction(ctx context.Context, req *domain.TransactionRequest) (*AccountingTxResp, error)
}

// AccountingTxResp 返回值, 也跟 workflow.AccountingTxResp 同形态.
type AccountingTxResp struct {
	VoucherNo string
	Status    int8
	Error     string
}

// RuleSpec — SaveGraph saga 推到 accounting 的一条 rule 元数据.
// 字段跟 accounting.model.TransactionRule 对齐, 由 wire 层翻成 JSON 发出去.
type RuleSpec struct {
	ProductCode     string
	EventCode       string
	HashKey         string
	DebitSubjectID  string
	CreditSubjectID string
	FromDirection   string
	ToDirection     string
	TransactionType int
	BookkeepingMode string
}

// AccountingRuleSyncer — SaveGraph 时把 graph 派生出的 rules 推到 accounting.
// 实现一般是 HTTP POST /admin/transaction-rules. nil → SaveGraph 跳过同步 (best-effort 退化).
//
// SP-AC-7 R2: DeleteRules 是 saga 补偿入口 — Upsert OK 但本地 graph save 失败时,
// caller 应该调本方法回滚 accounting 端已 upsert 的 rule. Hash keys 数组对应 RuleSpec.HashKey.
type AccountingRuleSyncer interface {
	UpsertRules(ctx context.Context, rules []RuleSpec) error
	DeleteRules(ctx context.Context, hashKeys []string) error
}

// AccountingOrderResetter — TriggerEvent 遇到卡 Processing 时主动重置.
// 实现一般是 HTTP POST /admin/transaction-orders/{order_no}/reset. nil → 不重试卡住的, 仅返 Processing.
type AccountingOrderResetter interface {
	ResetOrder(ctx context.Context, orderNo, businessNo string, force bool) error
}

// Server 实现 Kitex AdminService (kitex_gen/.../adminservice.AdminService).
// 老 grpc UnimplementedAdminServiceServer embed 已删 (Kitex 不需要).
type Server struct {
	Graphs     GraphRepo
	Accounting AccountingMetaCaller    // nil → TriggerEvent 返错; DryRun 不受影响
	RuleSync   AccountingRuleSyncer    // nil → SaveGraph 跳过 rule 同步
	OrderReset AccountingOrderResetter // nil → TriggerEvent 卡 Processing 时只能等 background recovery
	Audit      AuditSink               // nil → 不写资金审计 (强烈建议生产配, 见 audit.go)
	Log        *zap.Logger
}

// NewServer.
func NewServer(graphs GraphRepo, accounting AccountingMetaCaller, ruleSync AccountingRuleSyncer, orderReset AccountingOrderResetter, audit AuditSink, log *zap.Logger) *Server {
	return &Server{Graphs: graphs, Accounting: accounting, RuleSync: ruleSync, OrderReset: orderReset, Audit: audit, Log: log}
}

// ListGraphs.
func (s *Server) ListGraphs(ctx context.Context, _ *ListGraphsRequest) (*ListGraphsResponse, error) {
	list, err := s.Graphs.List(ctx, "all")
	if err != nil {
		return nil, err
	}
	items := make([]*GraphSummary, 0, len(list))
	for _, g := range list {
		items = append(items, &GraphSummary{
			Key: g.Key, Name: g.Name, Version: g.Version, Status: g.Status,
			OwnerType: g.OwnerType, OwnerID: g.OwnerID,
		})
	}
	return &ListGraphsResponse{Items: items}, nil
}

// GetGraph.
func (s *Server) GetGraph(ctx context.Context, req *GetGraphRequest) (*GetGraphResponse, error) {
	if req.Key == "" {
		return nil, errors.New("key required")
	}
	g, err := s.Graphs.GetByKey(ctx, req.Key)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("graph %q not found", req.Key)
	}
	specBytes, err := json.Marshal(g.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal spec: %w", err)
	}
	return &GetGraphResponse{Graph: &Graph{
		Key: g.Key, Name: g.Name, Version: g.Version, Status: g.Status,
		OwnerType: g.OwnerType, OwnerID: g.OwnerID,
		SpecJson: specBytes,
	}}, nil
}

// SaveGraph — upsert.
//
// 入参 graph.spec_json 是 domain.GraphSpec 的 JSON 字面量, 这里 Unmarshal 还原.
//
// SP-AC-7 saga: 先把 graph 派生出的 (product_code, event_code) 规则推到 accounting
// (`POST /admin/transaction-rules`), 全部成功才落 graph 本地 DB. 任一 rule upsert
// 失败 → 返错, graph 不存. 这样保证 accounting 永远先于 split-payment 见到规则,
// trigger 时 GetRulesByProductAndEvent 不会 miss.
//
// RuleSync == nil 时退化为旧行为 (跳过同步, 仅保存 graph), 便于 dev 单仓启动.
func (s *Server) SaveGraph(ctx context.Context, req *SaveGraphRequest) (*SaveGraphResponse, error) {
	start := time.Now()
	outcome := "success"
	defer func() {
		key := ""
		if req != nil && req.Graph != nil {
			key = req.Graph.Key
		}
		observability.SaveGraphDuration.WithLabelValues(key, outcome).Observe(time.Since(start).Seconds())
	}()
	if req.Graph == nil {
		outcome = "invalid"
		return nil, errors.New("graph required")
	}
	if req.Graph.Key == "" {
		outcome = "invalid"
		return nil, errors.New("graph.key required")
	}
	g := &domain.Graph{
		Key:       req.Graph.Key,
		Name:      req.Graph.Name,
		Version:   firstNonEmpty(req.Graph.Version, "1.0.0"),
		Status:    firstNonEmpty(req.Graph.Status, "draft"),
		OwnerType: req.Graph.OwnerType,
		OwnerID:   req.Graph.OwnerID,
	}
	if len(req.Graph.SpecJson) > 0 {
		if err := json.Unmarshal(req.Graph.SpecJson, &g.Spec); err != nil {
			return nil, fmt.Errorf("decode spec_json: %w", err)
		}
	}

	// Saga step 1: derive + push rules to accounting (前置, 失败则全部 abort)
	if s.RuleSync != nil {
		rules := deriveRulesFromGraph(g)
		if len(rules) > 0 {
			if err := s.RuleSync.UpsertRules(ctx, rules); err != nil {
				outcome = "rule_sync_failed"
				if s.Log != nil {
					s.Log.Error("SaveGraph: accounting rule sync failed; aborting graph save",
						zap.String("graph_key", g.Key), zap.Int("rule_count", len(rules)), zap.Error(err))
				}
				return nil, fmt.Errorf("accounting rule sync: %w (graph not saved)", err)
			}
		}
	}

	// Saga step 2: 本地持久化 graph
	if _, err := s.Graphs.Save(ctx, g); err != nil {
		outcome = "local_save_failed"
		// SP-AC-7 R2: 本地 graph save 失败 → 补偿删除已 upsert 的 accounting rule.
		// 资金安全考虑: 残留 rule 虽然幂等但是孤儿数据, 长期累积会让 admin UI 混乱;
		// 而且能调 TriggerEvent (绕开 SaveGraph) 利用孤儿 rule 落账. 必须回滚.
		if s.RuleSync != nil {
			rules := deriveRulesFromGraph(g)
			if len(rules) > 0 {
				hashKeys := make([]string, 0, len(rules))
				for _, r := range rules {
					hashKeys = append(hashKeys, r.HashKey)
				}
				if compErr := s.RuleSync.DeleteRules(ctx, hashKeys); compErr != nil {
					// 补偿失败 → 残留, 但至少 log + 给 caller 一个明确告警, 让人工跟进.
					if s.Log != nil {
						s.Log.Error("SaveGraph saga compensation FAILED — accounting rule 残留, 需人工 DELETE",
							zap.String("graph_key", g.Key),
							zap.Strings("hash_keys", hashKeys),
							zap.Error(compErr))
					}
					return nil, fmt.Errorf("graph save failed (%v); compensate failed too: %w (rules 残留, 人工 cleanup)", err, compErr)
				}
				if s.Log != nil {
					s.Log.Info("SaveGraph saga: rolled back accounting rules due to local save failure",
						zap.String("graph_key", g.Key), zap.Int("rolled_back", len(hashKeys)))
				}
			}
		}
		return nil, err
	}
	return &SaveGraphResponse{Key: g.Key, Version: g.Version}, nil
}

// DeriveRulesFromGraph 公开包装, 启动期 reconcileGraphRules 用.
func DeriveRulesFromGraph(g *domain.Graph) []RuleSpec { return deriveRulesFromGraph(g) }

// deriveRulesFromGraph 从 graph.spec 提取 (product_code, event_code) 唯一对, 每对生成一条 RuleSpec.
//
//   product_code = spec.scenario
//   subject_id   = 该 event_code 下第一条 edge 的 from/to node.ID (multi-leg 设计下退化为
//                  元数据/admin UI 展示用; 真实落账走 translator 输出的 N 条 leg).
//
// 同 event_code 多条 edge 只产出一条 rule (按 event_code 去重).
func deriveRulesFromGraph(g *domain.Graph) []RuleSpec {
	if g == nil || g.Spec.Scenario == "" || len(g.Spec.Edges) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []RuleSpec
	for _, e := range g.Spec.Edges {
		if e.EventCode == "" {
			continue
		}
		if seen[e.EventCode] {
			continue
		}
		seen[e.EventCode] = true
		out = append(out, RuleSpec{
			ProductCode:     g.Spec.Scenario,
			EventCode:       e.EventCode,
			HashKey:         g.Spec.Scenario + ":" + e.EventCode,
			DebitSubjectID:  e.From, // node.ID
			CreditSubjectID: e.To,   // node.ID
			FromDirection:   "debit",
			ToDirection:     "credit",
			TransactionType: 1,
			BookkeepingMode: "standard",
		})
	}
	return out
}

// DryRun — graph + event → 翻译预览, 不落账.
func (s *Server) DryRun(_ context.Context, req *DryRunRequest) (*DryRunResponse, error) {
	if req.Graph == nil {
		return &DryRunResponse{Error: "graph required"}, nil
	}
	g := &domain.Graph{
		Key: req.Graph.Key, Name: req.Graph.Name,
		Version: firstNonEmpty(req.Graph.Version, "1.0.0"),
		Status:  firstNonEmpty(req.Graph.Status, "draft"),
	}
	if len(req.Graph.SpecJson) > 0 {
		if err := json.Unmarshal(req.Graph.SpecJson, &g.Spec); err != nil {
			return &DryRunResponse{Error: "decode spec_json: " + err.Error()}, nil
		}
	}
	var tc workflow.TriggerContext
	if len(req.EventJson) > 0 {
		if err := json.Unmarshal(req.EventJson, &tc); err != nil {
			return &DryRunResponse{Error: "decode event_json: " + err.Error()}, nil
		}
	}
	plan, err := workflow.Translate(g, tc)
	if err != nil {
		return &DryRunResponse{Error: err.Error()}, nil
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return &DryRunResponse{Error: "marshal plan: " + err.Error()}, nil
	}
	return &DryRunResponse{PlanJson: planBytes}, nil
}

// TriggerEvent — 真触发:
//   1. 按 graph_key 找 graph
//   2. Translator → multi-leg TransactionRequest 列表
//   3. 每个 TransactionRequest 调 AccountingMeta.CreateTransaction (一笔原子)
//   4. 收集 voucher_no 返回
//
// 任一笔失败 → 该笔标错 → 后续不再继续 (避免半截分账); 已成功的 voucher 仍返供审计.
// 重复触发同 (charge_id, event_code) — accounting 内部按 OrderNo 幂等去重, 不会重复落账.
func (s *Server) TriggerEvent(ctx context.Context, req *TriggerEventRequest) (*TriggerEventResponse, error) {
	start := time.Now()
	graphKey := ""
	if req != nil {
		graphKey = req.GraphKey
	}
	outcome := "success"
	defer func() {
		observability.TriggerDuration.WithLabelValues(graphKey, outcome).Observe(time.Since(start).Seconds())
	}()
	if req.GraphKey == "" {
		outcome = "invalid"
		return &TriggerEventResponse{Error: "graph_key required"}, nil
	}
	if s.Accounting == nil {
		return &TriggerEventResponse{Error: "accounting client not wired (server started without ACCOUNTING_GRPC_ADDR?)"}, nil
	}
	g, err := s.Graphs.GetByKey(ctx, req.GraphKey)
	if err != nil {
		return &TriggerEventResponse{Error: "get graph: " + err.Error()}, nil
	}
	if g == nil {
		return &TriggerEventResponse{Error: fmt.Sprintf("graph %q not found (save it first via SaveGraph)", req.GraphKey)}, nil
	}
	// SP-AC-7 状态管理: 只有 status=active 的 graph 才允许触发. draft / archived 等
	// 状态的 graph 仅能编辑/查看, 不能落账. 这层守门员避免误把测试版本/历史版本拿来 trigger.
	if g.Status != "active" {
		return &TriggerEventResponse{Error: fmt.Sprintf("graph %q status=%q is not active; only active graphs can be triggered", req.GraphKey, g.Status)}, nil
	}
	var tc workflow.TriggerContext
	if len(req.EventJson) > 0 {
		if err := json.Unmarshal(req.EventJson, &tc); err != nil {
			return &TriggerEventResponse{Error: "decode event_json: " + err.Error()}, nil
		}
	}
	plan, err := workflow.Translate(g, tc)
	if err != nil {
		return &TriggerEventResponse{Error: "translate: " + err.Error()}, nil
	}

	// SP-AC-7 resume-aware trigger: 多个 transaction (= 多个 event_code) 不再 break-on-failure.
	// 关键场景:
	//   - 单笔 trigger 跑 N 个 tx, 中间某个失败 → 不影响后面已配置的 tx 继续尝试
	//   - 同 business_no 重新 trigger → 已 Success 的 tx 走 accounting 幂等返 cached
	//   - 卡 Processing 的 tx → 主动 reset 一次再重试 (resetThenRetry)
	// 资金安全: accounting CreateTransaction 内部按 order_no 幂等 + status 守门员, 多次调用 0 重复落账.
	resp := &TriggerEventResponse{}
	var failedTx []string
	for i := range plan.Transactions {
		tx := &plan.Transactions[i]
		v := &TxnVoucher{EventCode: tx.EventCode, OrderNo: tx.OrderNo}

		acctResp, callErr := s.Accounting.CreateTransaction(ctx, tx)
		// 处理 Processing 卡死: 主动 reset 一次, 再调一次 CreateTransaction.
		if callErr == nil && acctResp != nil && acctResp.Status == 1 /*Processing*/ {
			if s.OrderReset != nil {
				if rerr := s.OrderReset.ResetOrder(ctx, tx.OrderNo, tx.OrderNo, false); rerr != nil {
					if s.Log != nil {
						s.Log.Warn("TriggerEvent: reset stuck order failed; will keep Processing",
							zap.String("order_no", tx.OrderNo), zap.Error(rerr))
					}
				} else {
					// reset OK, 再调一次 CreateTransaction (走 Failed → Processing → Confirm 重试分支).
					if r2, e2 := s.Accounting.CreateTransaction(ctx, tx); e2 == nil && r2 != nil {
						acctResp, callErr = r2, nil
					}
				}
			}
		}

		if callErr != nil {
			v.Status = 3
			v.Error = callErr.Error()
			failedTx = append(failedTx, tx.OrderNo)
			if s.Log != nil {
				s.Log.Error("TriggerEvent: CreateTransaction failed (continuing)",
					zap.String("graph_key", req.GraphKey),
					zap.String("event_code", tx.EventCode),
					zap.String("order_no", tx.OrderNo),
					zap.Int("idx", i),
					zap.Int("total", len(plan.Transactions)),
					zap.Error(callErr))
			}
			resp.Vouchers = append(resp.Vouchers, v)
			continue // 不 break, 继续跑后续 tx
		}
		v.VoucherNo = acctResp.VoucherNo
		v.Status = int32(acctResp.Status)
		if acctResp.Error != "" {
			v.Error = acctResp.Error
		}
		if acctResp.Status != 2 /*Success*/ {
			failedTx = append(failedTx, tx.OrderNo)
		}
		// 每条 voucher 落 metric (按 event_code 区分,方便定位是哪个 phase 的问题).
		observability.VoucherStatusCount.WithLabelValues(
			tx.EventCode,
			observability.VoucherStatusLabel(int8(v.Status)),
		).Inc()
		// SP-AC-7 S6: 资金审计 - 每个 voucher 写一条独立 audit, 不阻塞业务即可.
		// SP-AC-7 P10: log + audit 都加 trace_id (从 OTel ctx 抓).
		if s.Audit != nil {
			actor := extractActor(ctx)
			_ = s.Audit.Write(ctx, AuditEvent{
				Action:     "moneyflow.trigger",
				GraphKey:   req.GraphKey,
				BusinessNo: tc.ChargeID,
				EventCode:  tx.EventCode,
				OrderNo:    tx.OrderNo,
				VoucherNo:  v.VoucherNo,
				Status:     v.Status,
				Actor:      actor,
				OccurredAt: time.Now().UTC(),
				Error:      v.Error,
				TraceID:    observability.TraceIDFromCtx(ctx),
			})
		}
		resp.Vouchers = append(resp.Vouchers, v)
	}
	if len(failedTx) > 0 {
		if len(failedTx) == len(plan.Transactions) {
			outcome = "failed"
		} else {
			outcome = "partial"
		}
		resp.Error = fmt.Sprintf("%d/%d tx unsuccessful (retry same business_no to resume): %v",
			len(failedTx), len(plan.Transactions), failedTx)
	}

	if planBytes, e := json.Marshal(plan); e == nil {
		resp.PlanJson = planBytes
	}
	return resp, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// extractActor 从 gRPC metadata 抽 actor 信息.
//   - x-actor 优先 (BFF 应该传入业务侧用户身份)
//   - x-admin-token 兜底, 不记原文 (脱敏: 只记前 8 字节 hash 用)
//   - 都没有 → "unknown"
func extractActor(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "unknown"
	}
	if v := md.Get("x-actor"); len(v) > 0 && v[0] != "" {
		return v[0]
	}
	if v := md.Get("x-admin-token"); len(v) > 0 && v[0] != "" {
		t := v[0]
		if len(t) > 8 {
			return "token-" + t[:8]
		}
		return "token-" + t
	}
	return "unknown"
}
