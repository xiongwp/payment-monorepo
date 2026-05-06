// http_api.go：SDK / admin REST API。
//
// 端点（全部 JSON in/out，除 watch 用 SSE）：
//
//   GET    /api/v1/configs/:ns/:key?instance_id=X
//          单 key 取值（命中 strategy + effective 窗口）；返 ConfigValue 或 404
//
//   PUT    /api/v1/configs/:ns/:key
//          admin 写新版本；body = PutVersionInput；返 {"version": <new>}
//
//   POST   /api/v1/configs/:ns/:key/rollback
//          body = {"to_version": N, "reason": "..."}; 返 {"version": <new>}
//
//   DELETE /api/v1/configs/:ns/:key?reason=...
//          软删
//
//   GET    /api/v1/configs/:ns/watch?instance_id=X&since_version=N
//          SSE long-stream；server-sent events 格式：
//             event: snapshot|update|delete
//             data: {"namespace":..., "key":..., "version":..., ...}
//          Snapshot 完后紧跟 realtime；客户端断 → 用 since_version 重连补齐。
//          (浏览器 / 老客户端走这里；服务间也可走这个，省掉 gRPC 依赖)
//
//   GET    /ws?namespace=...&instance_id=X&since_version=N
//          WebSocket；payload 同 SSE event。admin web 用。
//
// 鉴权：
//   - PUT / POST / DELETE：必须 actor (HTTP header X-Actor 或 ctx 由 middleware 注)
//   - GET / watch：mTLS client cert 已经 gating；不再二次 auth
//
// 错误码：
//   400 invalid input    | 401 missing actor | 404 namespace/key not found
//   409 version conflict | 500 server error
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/config-center/internal/metrics"
	"github.com/xiongwp/config-center/internal/service"
)

// SnapshotProducer service.Service 暴露给 HTTP 层的最小接口。
//
// 跟 service.Service 严格匹配；写本接口而不是直接用 *service.Service 是为
// admin 测试 / mock 方便。
type SnapshotProducer interface {
	GetConfig(ctx context.Context, namespace, key, instanceID string) (*service.ConfigRow, error)
	PutConfig(ctx context.Context, in service.PutVersionInput) (int64, error)
	Rollback(ctx context.Context, namespace, key string, toVersion int64, actor, reason string) (int64, error)
	WatchNamespace(ctx context.Context, namespace, instanceID string, sinceVersion int64) (<-chan *service.Event, error)
}

// HTTPAPI 把 service 暴露给 SDK / admin。
type HTTPAPI struct {
	svc       SnapshotProducer
	logger    *zap.Logger
	subsCount atomic.Int64 // 当前 SSE/WS 订阅数（gauge）
}

func NewHTTPAPI(svc SnapshotProducer, logger *zap.Logger) *HTTPAPI {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &HTTPAPI{svc: svc, logger: logger}
}

// Register 把 HTTP 路由挂到给定 mux。caller 在 main.go 调一次。
//
// 实现要点：
//   - 所有路径都用普通 net/http（不引入 gorilla/mux 依赖）
//   - 路径参数自己 trim：strings.TrimPrefix + Split
func (h *HTTPAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/configs/", h.routeConfigs)
}

// routeConfigs 总入口；按方法 + 路径分发。
//
//	/api/v1/configs/:ns/:key             GET / PUT / DELETE
//	/api/v1/configs/:ns/:key/rollback    POST
//	/api/v1/configs/:ns/watch            GET (SSE)
func (h *HTTPAPI) routeConfigs(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/configs/")
	parts := strings.Split(rest, "/")
	switch len(parts) {
	case 2:
		// /:ns/:key (or /:ns/watch)
		ns, second := parts[0], parts[1]
		if ns == "" {
			http.Error(w, "namespace required", http.StatusBadRequest)
			return
		}
		if second == "watch" {
			h.handleWatch(w, r, ns)
			return
		}
		h.handleSingleKey(w, r, ns, second)
	case 3:
		// /:ns/:key/rollback
		ns, key, action := parts[0], parts[1], parts[2]
		if action == "rollback" && r.Method == http.MethodPost {
			h.handleRollback(w, r, ns, key)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleSingleKey GET / PUT / DELETE on /:ns/:key。
func (h *HTTPAPI) handleSingleKey(w http.ResponseWriter, r *http.Request, ns, key string) {
	switch r.Method {
	case http.MethodGet:
		instanceID := r.URL.Query().Get("instance_id")
		if instanceID == "" {
			instanceID = "anon"
		}
		row, err := h.svc.GetConfig(r.Context(), ns, key, instanceID)
		if err != nil {
			h.logger.Warn("GetConfig failed", zap.Error(err))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if row == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, configRowToWire(row))

	case http.MethodPut:
		actor := actorFromRequest(r)
		if actor == "" {
			http.Error(w, "X-Actor required", http.StatusUnauthorized)
			return
		}
		var in putRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		newVer, err := h.svc.PutConfig(r.Context(), service.PutVersionInput{
			Namespace:    ns,
			Key:          key,
			Value:        in.Value,
			Format:       in.Format,
			EffectiveAt:  in.EffectiveAt,
			ExpireAt:     in.ExpireAt,
			Strategy:     in.Strategy,
			StrategySpec: in.StrategySpec,
			Actor:        actor,
			ChangeReason: in.ChangeReason,
		})
		if err != nil {
			h.logger.Warn("PutConfig failed", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		metrics.PutTotal.WithLabelValues("PUT").Inc()
		writeJSON(w, http.StatusOK, map[string]int64{"version": newVer})

	case http.MethodDelete:
		actor := actorFromRequest(r)
		if actor == "" {
			http.Error(w, "X-Actor required", http.StatusUnauthorized)
			return
		}
		// service.Service 没暴露 Delete；admin 通常软删走 PUT empty value。
		// 这里返 501 引导 caller 用 admin UI 的 delete 流（直接走 repo）。
		http.Error(w, "DELETE via admin UI only", http.StatusNotImplemented)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRollback POST /:ns/:key/rollback。
func (h *HTTPAPI) handleRollback(w http.ResponseWriter, r *http.Request, ns, key string) {
	actor := actorFromRequest(r)
	if actor == "" {
		http.Error(w, "X-Actor required", http.StatusUnauthorized)
		return
	}
	var in rollbackRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.ToVersion <= 0 {
		http.Error(w, "to_version required", http.StatusBadRequest)
		return
	}
	newVer, err := h.svc.Rollback(r.Context(), ns, key, in.ToVersion, actor, in.Reason)
	if err != nil {
		h.logger.Warn("Rollback failed", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	metrics.PutTotal.WithLabelValues("ROLLBACK").Inc()
	writeJSON(w, http.StatusOK, map[string]int64{"version": newVer})
}

// handleWatch SSE long stream。
//
// 事件格式：
//
//	event: snapshot|update|delete
//	id: <version>
//	data: {"namespace":"x","key":"y","version":N,"value":"...","format":"json",
//	       "strategy":"FULL","effective_at":"RFC3339"|null, ...}
//	\n\n
//
// 每 25s 发一个 ":heartbeat\n\n" comment 防中间 LB 砍连接。
//
// 客户端断开（http.ResponseWriter Push fail）→ ctx 自动 cancel → service 解订阅。
func (h *HTTPAPI) handleWatch(w http.ResponseWriter, r *http.Request, ns string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	instanceID := r.URL.Query().Get("instance_id")
	if instanceID == "" {
		instanceID = "anon"
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since_version"), 10, 64)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx 关 buffer

	ctx := r.Context()
	ch, err := h.svc.WatchNamespace(ctx, ns, instanceID, since)
	if err != nil {
		http.Error(w, "watch start: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.subsCount.Add(1)
	metrics.SubscribersGauge.WithLabelValues(ns).Inc()
	defer func() {
		h.subsCount.Add(-1)
		metrics.SubscribersGauge.WithLabelValues(ns).Dec()
	}()

	// flush header
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := w.Write([]byte(":hb\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev == nil || ev.Config == nil {
				continue
			}
			payload, err := json.Marshal(configRowToWire(ev.Config))
			if err != nil {
				h.logger.Warn("marshal event", zap.Error(err))
				continue
			}
			fmt.Fprintf(w, "event: %s\n", eventTypeName(ev.Type))
			fmt.Fprintf(w, "id: %d\n", ev.Config.Version)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
			metrics.PushTotal.WithLabelValues(ev.Config.Strategy).Inc()
		}
	}
}

// ─── wire types ──────────────────────────────────────────────────────────

// putRequest PUT body。
type putRequest struct {
	Value        string     `json:"value"`
	Format       string     `json:"format"`
	EffectiveAt  *time.Time `json:"effective_at,omitempty"`
	ExpireAt     *time.Time `json:"expire_at,omitempty"`
	Strategy     string     `json:"strategy"`        // FULL / CANARY / TARGETED / SCHEDULED
	StrategySpec string     `json:"strategy_spec"`   // CanarySpec / TargetedSpec JSON
	ChangeReason string     `json:"change_reason"`
}

type rollbackRequest struct {
	ToVersion int64  `json:"to_version"`
	Reason    string `json:"reason"`
}

// ConfigWire SDK / admin 看到的 JSON 形态。
type ConfigWire struct {
	Namespace    string     `json:"namespace"`
	Key          string     `json:"key"`
	Version      int64      `json:"version"`
	Value        string     `json:"value"`
	Format       string     `json:"format"`
	EffectiveAt  *time.Time `json:"effective_at,omitempty"`
	ExpireAt     *time.Time `json:"expire_at,omitempty"`
	Strategy     string     `json:"strategy"`
	StrategySpec string     `json:"strategy_spec,omitempty"`
	UpdatedBy    string     `json:"updated_by"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func configRowToWire(r *service.ConfigRow) *ConfigWire {
	if r == nil {
		return nil
	}
	return &ConfigWire{
		Namespace:    r.Namespace,
		Key:          r.KeyName,
		Version:      r.Version,
		Value:        r.Value,
		Format:       r.Format,
		EffectiveAt:  r.EffectiveAt,
		ExpireAt:     r.ExpireAt,
		Strategy:     r.Strategy,
		StrategySpec: r.StrategySpec,
		UpdatedBy:    r.CreatedBy,
		UpdatedAt:    r.CreatedAt,
	}
}

func eventTypeName(t service.EventType) string {
	switch t {
	case service.EventSnapshot:
		return "snapshot"
	case service.EventUpdate:
		return "update"
	case service.EventDelete:
		return "delete"
	}
	return "unknown"
}

// ─── helpers ──────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// actorFromRequest admin middleware 应在 ctx 注（用 server.WithActor 设的同一
// ctxActorKey）；fallback 看 X-Actor header（dev / curl 用）。
// 生产 mTLS gating 后 caller cert CN 可作为 actor。
func actorFromRequest(r *http.Request) string {
	if a, ok := r.Context().Value(ctxActorKey).(string); ok && a != "" {
		return a
	}
	return r.Header.Get("X-Actor")
}

// ErrNotFound REST 用；service.GetConfig 返 nil 时映射到 404。
var ErrNotFound = errors.New("not found")
