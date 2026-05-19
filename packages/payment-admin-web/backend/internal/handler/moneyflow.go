// Package handler — moneyflow.go (RESTORE-9 真接).
//
// payment-admin-web 的 Money Flow Designer BFF: SPA 调本 handler /api/moneyflow/*,
// 这里转 Kitex 调 split-payment.AdminService.
//
// 上游 split-payment kitex_gen 已生成 (RESTORE-6 完成).
package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/transport"

	splitv1 "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1"
	adminservice "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1/adminservice"
)

// MoneyflowHandler 通过 Kitex 调 split-payment.AdminService.
type MoneyflowHandler struct {
	cli    adminservice.Client
	once   sync.Once
	dialEr error
}

// NewMoneyflowHandler 构造. lazy dial — 第一次访问时拨号, 避免 split-payment
// 还没起来时本 backend 起不来.
func NewMoneyflowHandler() *MoneyflowHandler { return &MoneyflowHandler{} }

func (h *MoneyflowHandler) ensure() error {
	h.once.Do(func() {
		addr := os.Getenv("SPLIT_PAYMENT_GRPC_ADDR")
		if addr == "" {
			addr = "split-payment:9098"
		}
		cli, err := adminservice.NewClient("split-payment",
			client.WithHostPorts(addr),
			client.WithTransportProtocol(transport.GRPC),			client.WithRPCTimeout(15*time.Second),
		)
		if err != nil {
			h.dialEr = err
			return
		}
		h.cli = cli
	})
	return h.dialEr
}

func (h *MoneyflowHandler) fail(w http.ResponseWriter, code int, msg string) {
	http.Error(w, `{"error":`+strconv.Quote(msg)+`}`, code)
}

// Proxy /api/moneyflow/* — 按 method + path 路由到 split-payment RPC.
func (h *MoneyflowHandler) Proxy(w http.ResponseWriter, r *http.Request) {
	if err := h.ensure(); err != nil {
		h.fail(w, http.StatusServiceUnavailable, "split-payment dial: "+err.Error())
		return
	}
	// Path = /api/moneyflow/<sub>... — 取 <sub> 第一段.
	tail := strings.TrimPrefix(r.URL.Path, "/api/moneyflow/")
	parts := strings.SplitN(tail, "/", 2)
	op := parts[0]
	switch op {
	case "graphs":
		h.handleGraphs(w, r, parts)
	case "dry-run":
		h.handleDryRun(w, r)
	case "trigger":
		h.handleTrigger(w, r)
	default:
		h.fail(w, http.StatusNotFound, "unknown moneyflow op: "+op)
	}
}

func (h *MoneyflowHandler) handleGraphs(w http.ResponseWriter, r *http.Request, parts []string) {
	// /api/moneyflow/graphs        — GET 列表 / POST upsert
	// /api/moneyflow/graphs/<key>  — GET 详情 / DELETE 删
	hasKey := len(parts) > 1 && parts[1] != ""
	switch {
	case r.Method == http.MethodGet && !hasKey:
		resp, err := h.cli.ListGraphs(r.Context(), &splitv1.ListGraphsRequest{})
		if err != nil {
			h.fail(w, http.StatusBadGateway, "ListGraphs: "+err.Error())
			return
		}
		writeJSON(w, resp)
	case r.Method == http.MethodGet && hasKey:
		resp, err := h.cli.GetGraph(r.Context(), &splitv1.GetGraphRequest{Key: parts[1]})
		if err != nil {
			h.fail(w, http.StatusBadGateway, "GetGraph: "+err.Error())
			return
		}
		writeJSON(w, resp)
	case r.Method == http.MethodPost && !hasKey:
		body, _ := io.ReadAll(r.Body)
		var g splitv1.Graph
		if err := json.Unmarshal(body, &g); err != nil {
			h.fail(w, http.StatusBadRequest, "decode graph: "+err.Error())
			return
		}
		resp, err := h.cli.SaveGraph(r.Context(), &splitv1.SaveGraphRequest{Graph: &g})
		if err != nil {
			h.fail(w, http.StatusBadGateway, "SaveGraph: "+err.Error())
			return
		}
		writeJSON(w, resp)
	case r.Method == http.MethodDelete && hasKey:
		resp, err := h.cli.DeleteGraph(r.Context(), &splitv1.DeleteGraphRequest{Key: parts[1]})
		if err != nil {
			h.fail(w, http.StatusBadGateway, "DeleteGraph: "+err.Error())
			return
		}
		writeJSON(w, resp)
	default:
		h.fail(w, http.StatusMethodNotAllowed, "unsupported "+r.Method+" on /graphs")
	}
}

type dryRunBody struct {
	Graph splitv1.Graph   `json:"graph"`
	Event json.RawMessage `json:"event"`
}

func (h *MoneyflowHandler) handleDryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.fail(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var br dryRunBody
	if err := json.Unmarshal(body, &br); err != nil {
		h.fail(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	resp, err := h.cli.DryRun(r.Context(), &splitv1.DryRunRequest{
		Graph:     &br.Graph,
		EventJson: []byte(br.Event),
	})
	if err != nil {
		h.fail(w, http.StatusBadGateway, "DryRun: "+err.Error())
		return
	}
	writeJSON(w, resp)
}

type triggerBody struct {
	GraphKey string          `json:"graph_key"`
	Event    json.RawMessage `json:"event"`
}

func (h *MoneyflowHandler) handleTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.fail(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var br triggerBody
	if err := json.Unmarshal(body, &br); err != nil {
		h.fail(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	resp, err := h.cli.TriggerEvent(r.Context(), &splitv1.TriggerEventRequest{
		GraphKey:  br.GraphKey,
		EventJson: []byte(br.Event),
	})
	if err != nil {
		h.fail(w, http.StatusBadGateway, "TriggerEvent: "+err.Error())
		return
	}
	writeJSON(w, resp)
}

// Designer / Resources / DesignerV2 / Rules — SPA 静态页, 由前端路由处理.
// 现在 split-payment 接通后这几个端点改成简单的 redirect / 占位响应,
// 真正前端代码在 web/ 目录里 (vite build 后挂在 /admin/*).
func (h *MoneyflowHandler) Designer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Money Flow Designer</title>
<body><h1>Money Flow Designer</h1>
<p>SPA route — go through /admin/moneyflow/designer via the React app.</p>
<p>BFF API: <code>/api/moneyflow/graphs</code>, <code>/api/moneyflow/dry-run</code>, <code>/api/moneyflow/trigger</code></p>
</body>`))
}
func (h *MoneyflowHandler) Resources(w http.ResponseWriter, r *http.Request)  { h.Designer(w, r) }
func (h *MoneyflowHandler) DesignerV2(w http.ResponseWriter, r *http.Request) { h.Designer(w, r) }
func (h *MoneyflowHandler) Rules(w http.ResponseWriter, r *http.Request)      { h.Designer(w, r) }

// Health 上报 split-payment 拨号状态.
func (h *MoneyflowHandler) Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := h.ensure(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"down","reason":"` + err.Error() + `"}`))
		return
	}
	_, _ = w.Write([]byte(`{"status":"ok","service":"moneyflow"}`))
}
