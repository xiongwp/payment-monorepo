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
//
// 注: 没要求 Delete — 现有 MemoryGraphRepo / MySQLGraphRepo 都不暴露删除方法
// (graph 版本控制要求软删除, 不支持硬删). DeleteGraph gRPC 用 UnimplementedAdminServiceServer
// 兜底返 Unimplemented.
type GraphRepo interface {
	Save(ctx context.Context, g *domain.Graph) (int64, error)
	GetByKey(ctx context.Context, key string) (*domain.Graph, error)
	List(ctx context.Context, status string) ([]*domain.Graph, error)
}

// Server 实现 AdminServiceServer.
type Server struct {
	UnimplementedAdminServiceServer
	Graphs GraphRepo
	Log    *zap.Logger
}

// NewServer.
func NewServer(graphs GraphRepo, log *zap.Logger) *Server {
	return &Server{Graphs: graphs, Log: log}
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
