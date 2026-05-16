// moneyflow.go — MF-2 / SP-AC-7 Money Flow Designer BFF.
//
// SP-AC-7 重构: split-payment 改成纯内部 gRPC 服务, 这里从 HTTP reverse-proxy
// 切换成 gRPC client + JSON bridge:
//
//   Browser
//     │ HTTP /api/moneyflow/*
//     ▼
//   admin-web BFF (本文件)  —— hand-written gRPC client
//     │ gRPC /split_payment.v1.AdminService/*
//     ▼
//   split-payment (gRPC port :9098)
//
// 跟 split-payment/internal/grpcsvc/admin_service.go 互通靠:
//   - 同 ServiceName + 方法路径
//   - 同 protobuf 字段 tag (proto3 wire format)
// 不依赖 vendored pb 代码 — hand-written 避免 internal/ 包跨模块引用问题.
package handler

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// designerHTML — MF-2 frontend designer 直接 embed 进二进制.
//
//go:embed assets/moneyflow-designer.html
var designerHTML []byte

//go:embed assets/moneyflow-resources.html
var resourcesHTML []byte

//go:embed assets/moneyflow-designer-v2.html
var designerV2HTML []byte

//go:embed assets/moneyflow-rules.html
var rulesHTML []byte

// ─── hand-written gRPC client types (跟 split-payment grpcsvc 端形态一致) ──

type rpcGraphSummary struct {
	Key       string `protobuf:"bytes,1,opt,name=key,proto3"`
	Name      string `protobuf:"bytes,2,opt,name=name,proto3"`
	Version   string `protobuf:"bytes,3,opt,name=version,proto3"`
	Status    string `protobuf:"bytes,4,opt,name=status,proto3"`
	OwnerType string `protobuf:"bytes,5,opt,name=owner_type,json=ownerType,proto3"`
	OwnerID   string `protobuf:"bytes,6,opt,name=owner_id,json=ownerId,proto3"`
}

func (x *rpcGraphSummary) Reset()         { *x = rpcGraphSummary{} }
func (x *rpcGraphSummary) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcGraphSummary) ProtoMessage()    {}

type rpcGraph struct {
	Key       string `protobuf:"bytes,1,opt,name=key,proto3"`
	Name      string `protobuf:"bytes,2,opt,name=name,proto3"`
	Version   string `protobuf:"bytes,3,opt,name=version,proto3"`
	Status    string `protobuf:"bytes,4,opt,name=status,proto3"`
	OwnerType string `protobuf:"bytes,5,opt,name=owner_type,json=ownerType,proto3"`
	OwnerID   string `protobuf:"bytes,6,opt,name=owner_id,json=ownerId,proto3"`
	SpecJson  []byte `protobuf:"bytes,7,opt,name=spec_json,json=specJson,proto3"`
}

func (x *rpcGraph) Reset()         { *x = rpcGraph{} }
func (x *rpcGraph) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcGraph) ProtoMessage()    {}

type rpcListGraphsRequest struct{}

func (x *rpcListGraphsRequest) Reset()         { *x = rpcListGraphsRequest{} }
func (x *rpcListGraphsRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcListGraphsRequest) ProtoMessage()    {}

type rpcListGraphsResponse struct {
	Items []*rpcGraphSummary `protobuf:"bytes,1,rep,name=items,proto3"`
}

func (x *rpcListGraphsResponse) Reset()         { *x = rpcListGraphsResponse{} }
func (x *rpcListGraphsResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcListGraphsResponse) ProtoMessage()    {}

type rpcGetGraphRequest struct {
	Key string `protobuf:"bytes,1,opt,name=key,proto3"`
}

func (x *rpcGetGraphRequest) Reset()         { *x = rpcGetGraphRequest{} }
func (x *rpcGetGraphRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcGetGraphRequest) ProtoMessage()    {}

type rpcGetGraphResponse struct {
	Graph *rpcGraph `protobuf:"bytes,1,opt,name=graph,proto3"`
}

func (x *rpcGetGraphResponse) Reset()         { *x = rpcGetGraphResponse{} }
func (x *rpcGetGraphResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcGetGraphResponse) ProtoMessage()    {}

type rpcSaveGraphRequest struct {
	Graph *rpcGraph `protobuf:"bytes,1,opt,name=graph,proto3"`
}

func (x *rpcSaveGraphRequest) Reset()         { *x = rpcSaveGraphRequest{} }
func (x *rpcSaveGraphRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcSaveGraphRequest) ProtoMessage()    {}

type rpcSaveGraphResponse struct {
	Key     string `protobuf:"bytes,1,opt,name=key,proto3"`
	Version string `protobuf:"bytes,2,opt,name=version,proto3"`
}

func (x *rpcSaveGraphResponse) Reset()         { *x = rpcSaveGraphResponse{} }
func (x *rpcSaveGraphResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcSaveGraphResponse) ProtoMessage()    {}

type rpcDryRunRequest struct {
	Graph     *rpcGraph `protobuf:"bytes,1,opt,name=graph,proto3"`
	EventJson []byte    `protobuf:"bytes,2,opt,name=event_json,json=eventJson,proto3"`
}

func (x *rpcDryRunRequest) Reset()         { *x = rpcDryRunRequest{} }
func (x *rpcDryRunRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcDryRunRequest) ProtoMessage()    {}

type rpcDryRunResponse struct {
	PlanJson []byte `protobuf:"bytes,1,opt,name=plan_json,json=planJson,proto3"`
	Error    string `protobuf:"bytes,2,opt,name=error,proto3"`
}

func (x *rpcDryRunResponse) Reset()         { *x = rpcDryRunResponse{} }
func (x *rpcDryRunResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcDryRunResponse) ProtoMessage()    {}

// TriggerEvent wire 类型.
type rpcTriggerEventRequest struct {
	GraphKey  string `protobuf:"bytes,1,opt,name=graph_key,json=graphKey,proto3"`
	EventJson []byte `protobuf:"bytes,2,opt,name=event_json,json=eventJson,proto3"`
}

func (x *rpcTriggerEventRequest) Reset()         { *x = rpcTriggerEventRequest{} }
func (x *rpcTriggerEventRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcTriggerEventRequest) ProtoMessage()    {}

type rpcTxnVoucher struct {
	EventCode string `protobuf:"bytes,1,opt,name=event_code,json=eventCode,proto3"`
	OrderNo   string `protobuf:"bytes,2,opt,name=order_no,json=orderNo,proto3"`
	VoucherNo string `protobuf:"bytes,3,opt,name=voucher_no,json=voucherNo,proto3"`
	Status    int32  `protobuf:"varint,4,opt,name=status,proto3"`
	Error     string `protobuf:"bytes,5,opt,name=error,proto3"`
}

func (x *rpcTxnVoucher) Reset()         { *x = rpcTxnVoucher{} }
func (x *rpcTxnVoucher) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcTxnVoucher) ProtoMessage()    {}

type rpcTriggerEventResponse struct {
	Vouchers []*rpcTxnVoucher `protobuf:"bytes,1,rep,name=vouchers,proto3"`
	Error    string           `protobuf:"bytes,2,opt,name=error,proto3"`
	PlanJson []byte           `protobuf:"bytes,3,opt,name=plan_json,json=planJson,proto3"`
}

func (x *rpcTriggerEventResponse) Reset()         { *x = rpcTriggerEventResponse{} }
func (x *rpcTriggerEventResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*rpcTriggerEventResponse) ProtoMessage()    {}

// ─── MoneyflowHandler ──────────────────────────────────────────────────────

// MoneyflowHandler — gRPC client to split-payment.
type MoneyflowHandler struct {
	upstream string
	conn     *grpc.ClientConn
	connMu   sync.Mutex
}

// NewMoneyflowHandler.
//
// upstream 走 env SPLIT_PAYMENT_GRPC_ADDR (默认 split-payment:9098).
// gRPC conn 按需 lazy 建立 (避免 split-payment 没起时本服务起不来).
func NewMoneyflowHandler() *MoneyflowHandler {
	up := envOr("SPLIT_PAYMENT_GRPC_ADDR", "split-payment:9098")
	return &MoneyflowHandler{upstream: up}
}

// getConn 懒加载 gRPC 连接, 失败时返 nil + error (handler 转 502).
//
// 用 passthrough:/// scheme 而不是默认 dns:/// :
//   - grpc-go 1.67+ 默认 dns resolver 对 Docker 的 host.docker.internal / 服务名
//     有时返 "produced zero addresses" 错误 (尤其 Linux 上 hostgateway 解析时序)
//   - passthrough 跳过 gRPC 自己的 DNS, 直接把 host:port 传给 net.Dial,
//     让系统层 (libc + Docker embedded DNS) 解析, 行为更可预测
func (h *MoneyflowHandler) getConn() (*grpc.ClientConn, error) {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.conn != nil {
		return h.conn, nil
	}
	if h.upstream == "" {
		return nil, fmt.Errorf("SPLIT_PAYMENT_GRPC_ADDR not configured")
	}
	target := h.upstream
	if !strings.Contains(target, ":///") {
		target = "passthrough:///" + target
	}
	conn, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", target, err)
	}
	h.conn = conn
	return conn, nil
}

// Proxy mux 注册: /api/moneyflow/* → 路由到 gRPC 方法.
//
//   GET    /api/moneyflow/graphs         → AdminService.ListGraphs
//   GET    /api/moneyflow/graphs/{key}   → AdminService.GetGraph
//   POST   /api/moneyflow/graphs         → AdminService.SaveGraph
//   POST   /api/moneyflow/dry-run        → AdminService.DryRun
func (h *MoneyflowHandler) Proxy(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/moneyflow")
	switch {
	case path == "/graphs" && r.Method == http.MethodGet:
		h.handleListGraphs(w, r)
	case path == "/graphs" && r.Method == http.MethodPost:
		h.handleSaveGraph(w, r)
	case strings.HasPrefix(path, "/graphs/") && r.Method == http.MethodGet:
		h.handleGetGraph(w, r, strings.TrimPrefix(path, "/graphs/"))
	case path == "/dry-run" && r.Method == http.MethodPost:
		h.handleDryRun(w, r)
	case path == "/trigger" && r.Method == http.MethodPost:
		h.handleTrigger(w, r)
	default:
		http.Error(w, "unknown moneyflow endpoint: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (h *MoneyflowHandler) handleListGraphs(w http.ResponseWriter, r *http.Request) {
	conn, err := h.getConn()
	if err != nil {
		http.Error(w, "split-payment unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	out := new(rpcListGraphsResponse)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, "/split_payment.v1.AdminService/ListGraphs",
		&rpcListGraphsRequest{}, out, grpc.StaticMethod()); err != nil {
		http.Error(w, "ListGraphs: "+err.Error(), http.StatusBadGateway)
		return
	}
	// 兼容老 designer 期望的 shape: {data: [...]}
	list := make([]map[string]any, 0, len(out.Items))
	for _, it := range out.Items {
		list = append(list, map[string]any{
			"key": it.Key, "name": it.Name, "version": it.Version,
			"status": it.Status, "owner_type": it.OwnerType, "owner_id": it.OwnerID,
		})
	}
	writeJSON(w, map[string]any{"data": list})
}

func (h *MoneyflowHandler) handleGetGraph(w http.ResponseWriter, r *http.Request, key string) {
	conn, err := h.getConn()
	if err != nil {
		http.Error(w, "split-payment unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	out := new(rpcGetGraphResponse)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, "/split_payment.v1.AdminService/GetGraph",
		&rpcGetGraphRequest{Key: key}, out, grpc.StaticMethod()); err != nil {
		http.Error(w, "GetGraph: "+err.Error(), http.StatusBadGateway)
		return
	}
	if out.Graph == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	resp := map[string]any{
		"key": out.Graph.Key, "name": out.Graph.Name, "version": out.Graph.Version,
		"status": out.Graph.Status,
	}
	if len(out.Graph.SpecJson) > 0 {
		var spec any
		_ = json.Unmarshal(out.Graph.SpecJson, &spec)
		resp["spec"] = spec
	}
	writeJSON(w, map[string]any{"data": resp})
}

// handleSaveGraph — designer Save 时 POST 整个 graph (含 spec). 这里把 spec 重新 marshal
// 进 bytes spec_json 字段, 透传给 split-payment.
func (h *MoneyflowHandler) handleSaveGraph(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var raw struct {
		Key       string          `json:"key"`
		Name      string          `json:"name"`
		Version   string          `json:"version"`
		Status    string          `json:"status"`
		OwnerType string          `json:"owner_type"`
		OwnerID   string          `json:"owner_id"`
		Spec      json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "parse json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if raw.Key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	conn, err := h.getConn()
	if err != nil {
		http.Error(w, "split-payment unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	out := new(rpcSaveGraphResponse)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	in := &rpcSaveGraphRequest{Graph: &rpcGraph{
		Key:       raw.Key,
		Name:      raw.Name,
		Version:   raw.Version,
		Status:    raw.Status,
		OwnerType: raw.OwnerType,
		OwnerID:   raw.OwnerID,
		SpecJson:  raw.Spec,
	}}
	if err := conn.Invoke(ctx, "/split_payment.v1.AdminService/SaveGraph", in, out, grpc.StaticMethod()); err != nil {
		http.Error(w, "SaveGraph: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"key": out.Key, "version": out.Version})
}

// handleDryRun — body {graph, event} → 翻译预览.
func (h *MoneyflowHandler) handleDryRun(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var raw struct {
		Graph json.RawMessage `json:"graph"`
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "parse json: "+err.Error(), http.StatusBadRequest)
		return
	}
	// 从 graph object 里抽 spec 字段
	var graphRaw struct {
		Key, Name, Version, Status string
		Spec                       json.RawMessage `json:"spec"`
	}
	_ = json.Unmarshal(raw.Graph, &graphRaw)

	conn, err := h.getConn()
	if err != nil {
		http.Error(w, "split-payment unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	out := new(rpcDryRunResponse)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	in := &rpcDryRunRequest{
		Graph: &rpcGraph{
			Key: graphRaw.Key, Name: graphRaw.Name, Version: graphRaw.Version, Status: graphRaw.Status,
			SpecJson: graphRaw.Spec,
		},
		EventJson: raw.Event,
	}
	if err := conn.Invoke(ctx, "/split_payment.v1.AdminService/DryRun", in, out, grpc.StaticMethod()); err != nil {
		http.Error(w, "DryRun: "+err.Error(), http.StatusBadGateway)
		return
	}
	if out.Error != "" {
		writeJSON(w, map[string]any{"error": out.Error})
		return
	}
	var plan any
	_ = json.Unmarshal(out.PlanJson, &plan)
	writeJSON(w, map[string]any{"plan": plan})
}

// handleTrigger — 同步真触发. body: {graph_key, event}.
//   - graph_key: 用 split-payment DB 里已存的 graph (Save 过的)
//   - event:     workflow.TriggerContext 形态 (event/charge_id/amount_minor/currency/attributes)
//
// 跟 dry-run 区别: 这个会调 accounting.CreateTransaction 真落账, 返每个 event_code 的 voucher_no.
func (h *MoneyflowHandler) handleTrigger(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var raw struct {
		GraphKey string          `json:"graph_key"`
		Event    json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "parse json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if raw.GraphKey == "" {
		http.Error(w, "graph_key required", http.StatusBadRequest)
		return
	}
	conn, err := h.getConn()
	if err != nil {
		http.Error(w, "split-payment unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	out := new(rpcTriggerEventResponse)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	in := &rpcTriggerEventRequest{GraphKey: raw.GraphKey, EventJson: raw.Event}
	if err := conn.Invoke(ctx, "/split_payment.v1.AdminService/TriggerEvent", in, out, grpc.StaticMethod()); err != nil {
		http.Error(w, "TriggerEvent: "+err.Error(), http.StatusBadGateway)
		return
	}
	resp := map[string]any{
		"vouchers": out.Vouchers,
		"error":    out.Error,
	}
	if len(out.PlanJson) > 0 {
		var plan any
		_ = json.Unmarshal(out.PlanJson, &plan)
		resp["plan"] = plan
	}
	writeJSON(w, resp)
}

// Designer / Resources / DesignerV2 / Rules — 静态 HTML serve.
func (h *MoneyflowHandler) Designer(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, designerHTML)
}
func (h *MoneyflowHandler) Resources(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, resourcesHTML)
}
func (h *MoneyflowHandler) DesignerV2(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, designerV2HTML)
}
func (h *MoneyflowHandler) Rules(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, rulesHTML)
}

// Health GET /api/moneyflow/_health — 探 split-payment gRPC 是否可达.
func (h *MoneyflowHandler) Health(w http.ResponseWriter, _ *http.Request) {
	conn, err := h.getConn()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "upstream down: "+err.Error())
		return
	}
	// 用一次 ListGraphs 当 health probe (轻量)
	out := new(rpcListGraphsResponse)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, "/split_payment.v1.AdminService/ListGraphs",
		&rpcListGraphsRequest{}, out, grpc.StaticMethod()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "upstream probe failed: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"upstream": h.upstream, "status": "ok", "graphs": len(out.Items)})
}

// ─── helpers ───────────────────────────────────────────────────────────────

func writeHTML(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	_, _ = w.Write(b)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
