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

	"reconcile-system/packages/split-payment/internal/domain"
	"reconcile-system/packages/split-payment/internal/workflow"

	"go.uber.org/zap"
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

// Server 实现 AdminServiceServer.
type Server struct {
	UnimplementedAdminServiceServer
	Graphs     GraphRepo
	Accounting AccountingMetaCaller // nil → TriggerEvent 返错; DryRun 不受影响
	Log        *zap.Logger
}

// NewServer.
func NewServer(graphs GraphRepo, accounting AccountingMetaCaller, log *zap.Logger) *Server {
	return &Server{Graphs: graphs, Accounting: accounting, Log: log}
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
func (s *Server) SaveGraph(ctx context.Context, req *SaveGraphRequest) (*SaveGraphResponse, error) {
	if req.Graph == nil {
		return nil, errors.New("graph required")
	}
	if req.Graph.Key == "" {
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
	if _, err := s.Graphs.Save(ctx, g); err != nil {
		return nil, err
	}
	return &SaveGraphResponse{Key: g.Key, Version: g.Version}, nil
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
	if req.GraphKey == "" {
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

	resp := &TriggerEventResponse{}
	for i := range plan.Transactions {
		tx := &plan.Transactions[i]
		v := &TxnVoucher{EventCode: tx.EventCode, OrderNo: tx.OrderNo}
		acctResp, callErr := s.Accounting.CreateTransaction(ctx, tx)
		if callErr != nil {
			v.Status = 3
			v.Error = callErr.Error()
			resp.Vouchers = append(resp.Vouchers, v)
			resp.Error = fmt.Sprintf("tx %s (%s) failed: %s; %d/%d succeeded so far",
				tx.OrderNo, tx.EventCode, callErr.Error(), i, len(plan.Transactions))
			if s.Log != nil {
				s.Log.Error("TriggerEvent: CreateTransaction failed",
					zap.String("graph_key", req.GraphKey),
					zap.String("event_code", tx.EventCode),
					zap.String("order_no", tx.OrderNo),
					zap.Error(callErr))
			}
			break
		}
		v.VoucherNo = acctResp.VoucherNo
		v.Status = int32(acctResp.Status)
		if acctResp.Error != "" {
			v.Error = acctResp.Error
		}
		resp.Vouchers = append(resp.Vouchers, v)
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
