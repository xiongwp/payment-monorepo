// Package handler — moneyflow.go (RESTORE-9 真接).
//
// payment-admin-web 的 Money Flow Designer BFF: SPA 调本 handler /api/moneyflow/*,
// 这里转 Kitex 调 split-payment.AdminService.
//
// 上游 split-payment kitex_gen 已生成 (RESTORE-6 完成).
package handler

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/transport"
	"github.com/xiongwp/payment-util/kitexutil"

	splitv1 "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1"
	adminservice "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1/adminservice"
)

// MF-SERVE: embed 真 designer HTML assets, 替原来返 "SPA route" 占位 stub.
// 4 个 HTML 都在 ./assets/ 子目录, Designer/DesignerV2/Resources/Rules 各取一个.
//
//go:embed assets/moneyflow-designer.html assets/moneyflow-designer-v2.html assets/moneyflow-resources.html assets/moneyflow-rules.html
var moneyflowAssetsFS embed.FS

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
		// ETCD-5: 优先走 kitexutil.DefaultClientOptions ("split-payment" 注册名).
		// REGISTRY_ENDPOINTS 没配时回退到 SPLIT_PAYMENT_GRPC_ADDR 静态拨号.
		opts := kitexutil.DefaultClientOptions("split-payment")
		if addr := os.Getenv("SPLIT_PAYMENT_GRPC_ADDR"); addr != "" {
			opts = append(opts, client.WithHostPorts(addr))
		}
		opts = append(opts,
			client.WithTransportProtocol(transport.GRPC),
			client.WithRPCTimeout(15*time.Second),
		)
		cli, err := adminservice.NewClient("split-payment", opts...)
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
		// MF-SAVE-FIX: 前端发 {key, name, version, status, spec:{scenario,triggers,nodes,edges,...}}.
		// splitv1.Graph proto 只有 SpecJson []byte 字段 (没嵌套 Spec), 直接 Unmarshal 会
		// 把整个 spec 子对象丢了 → DB 里只剩 nil arrays. 拆两步:
		//   1. 解到松散 map, 把 spec 单独 marshal 进 SpecJson bytes
		//   2. 其余顶层字段 (key/name/version/status/owner_*) 复制到 Graph proto
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			h.fail(w, http.StatusBadRequest, "decode graph: "+err.Error())
			return
		}
		var g splitv1.Graph
		// 顶层 string 字段
		for _, f := range []struct {
			key string
			dst *string
		}{
			{"key", &g.Key},
			{"name", &g.Name},
			{"version", &g.Version},
			{"status", &g.Status},
			{"owner_type", &g.OwnerType},
			{"owner_id", &g.OwnerId},
		} {
			if v, ok := raw[f.key]; ok {
				_ = json.Unmarshal(v, f.dst)
			}
		}
		// spec 子对象 → SpecJson []byte (split-payment server 端再 json.Unmarshal 还原成 domain.GraphSpec)
		if specRaw, ok := raw["spec"]; ok && len(specRaw) > 0 {
			g.SpecJson = []byte(specRaw)
		}
		// 兼容: 如果前端直接发 "spec_json" 字段 (base64 或 string), 也接受.
		if sj, ok := raw["spec_json"]; ok && len(g.SpecJson) == 0 {
			var s string
			if json.Unmarshal(sj, &s) == nil && s != "" {
				g.SpecJson = []byte(s)
			} else {
				g.SpecJson = []byte(sj)
			}
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

// Designer / DesignerV2 / Resources / Rules — 真服务 embed 的 HTML asset.
// MF-SERVE: 之前所有 4 个 handler 都返同一 "SPA route" stub, 真 HTML 没接.
// 现在每个 handler 单独 serve 对应的 asset.
func (h *MoneyflowHandler) Designer(w http.ResponseWriter, r *http.Request) {
	serveMoneyflowAsset(w, r, "assets/moneyflow-designer.html")
}

func (h *MoneyflowHandler) DesignerV2(w http.ResponseWriter, r *http.Request) {
	serveMoneyflowAsset(w, r, "assets/moneyflow-designer-v2.html")
}

func (h *MoneyflowHandler) Resources(w http.ResponseWriter, r *http.Request) {
	serveMoneyflowAsset(w, r, "assets/moneyflow-resources.html")
}

func (h *MoneyflowHandler) Rules(w http.ResponseWriter, r *http.Request) {
	serveMoneyflowAsset(w, r, "assets/moneyflow-rules.html")
}

// serveMoneyflowAsset 从 embed FS 读出 HTML 直接吐 200. 找不到 → 500
// (开发期常见, 提示 ops 缺文件).
func serveMoneyflowAsset(w http.ResponseWriter, _ *http.Request, name string) {
	data, err := fs.ReadFile(moneyflowAssetsFS, name)
	if err != nil {
		http.Error(w, "moneyflow asset missing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	_, _ = w.Write(data)
}

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
