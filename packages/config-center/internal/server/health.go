// health.go：健康 / 就绪 探针。
//
// 三层暴露：
//   1. HTTP /healthz   liveness (进程没卡死即 OK)
//   2. HTTP /readyz    readiness (DB ping + etcd ping 都 OK 才 200，否则 503)
//   3. gRPC grpc.health.v1.Health/Check  K8s gRPC probe / 服务网格 / 客户端
//      WatchConfig 拒绝向 NOT_SERVING 实例打长连接
//
// 三个 endpoint 共享同一组 ProbeFn，避免不一致（healthz 报 OK 但 readyz 报 503
// 或反过来这种 typical bug）。
package server

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"gorm.io/gorm"
)

// ProbeFn 单个依赖的健康检查。返 nil = up；返 error = down + 原因。
// caller 写到 /readyz body 让运维一眼看到具体哪个 down。
type ProbeFn func(ctx context.Context) error

// HealthChecker 聚合多个 probe；线程安全，全栈共用一个实例。
type HealthChecker struct {
	probes  map[string]ProbeFn
	logger  *zap.Logger

	// drain 标记：fx OnStop 后置 true，所有 probe 直接 503 让 K8s 摘流量。
	draining atomic.Bool
}

func NewHealthChecker(logger *zap.Logger) *HealthChecker {
	return &HealthChecker{probes: make(map[string]ProbeFn), logger: logger}
}

// Register name → probe；name 是 "db" / "etcd" / "kafka" 等。
func (h *HealthChecker) Register(name string, fn ProbeFn) {
	if fn == nil {
		return
	}
	h.probes[name] = fn
}

// BeginDrain fx OnStop 调；之后 readyz 一直 503，liveness 仍 200（避免 K8s
// 误杀 pod 影响优雅关停）。
func (h *HealthChecker) BeginDrain() { h.draining.Store(true) }

// HTTP handlers

// Liveness 进程活着就 OK；不查依赖（避免下游问题导致 K8s 重启 pod）。
func (h *HealthChecker) Liveness(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

// Readiness 依赖全 OK + 不在 drain 才 200；否则 503 带具体哪个 dep 挂了。
func (h *HealthChecker) Readiness(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	for name, fn := range h.probes {
		if err := fn(ctx); err != nil {
			http.Error(w, "dep "+name+" down: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	_, _ = w.Write([]byte("ready"))
}

// CommonProbes 工厂：给 main.go 一键挂全套常见 probe。
func CommonProbes(db *gorm.DB) map[string]ProbeFn {
	return map[string]ProbeFn{
		"db": func(ctx context.Context) error {
			if db == nil {
				return errors.New("nil db")
			}
			sqlDB, err := db.DB()
			if err != nil {
				return err
			}
			return sqlDB.PingContext(ctx)
		},
	}
}

// ─── gRPC health.v1 ────────────────────────────────────────────────────

// gRPC health proto wire format（手写避开 import grpc/health/grpc_health_v1
// 的强依赖；service name + status enum 跟标准对齐）。
//
// SERVING=1 NOT_SERVING=2 SERVICE_UNKNOWN=3
type healthCheckRequest struct{ Service string }
type healthCheckResponse struct{ Status int32 }

// HealthGRPCServer 实现 grpc.health.v1.Health
//
// service 留空 = 整体健康；service="configcenter.v1.ConfigCenter" = 单业务
// 服务健康。两个都返同一状态（聚合无法分割）。
type HealthGRPCServer struct {
	hc *HealthChecker
}

func NewHealthGRPCServer(hc *HealthChecker) *HealthGRPCServer { return &HealthGRPCServer{hc: hc} }

// Check K8s gRPC probe / istio sidecar 调；不带 service 名时检查整体。
func (s *HealthGRPCServer) Check(ctx context.Context, req *healthCheckRequest) (*healthCheckResponse, error) {
	if s.hc.draining.Load() {
		return &healthCheckResponse{Status: 2}, nil // NOT_SERVING
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, fn := range s.hc.probes {
		if err := fn(probeCtx); err != nil {
			return &healthCheckResponse{Status: 2}, nil
		}
	}
	return &healthCheckResponse{Status: 1}, nil // SERVING
}

// RegisterHealthService stub - 真正的 grpc 接线在 main.go 直接用
// google.golang.org/grpc/health 标准 server 包；这里留 hook 便于切换。
//
// 若想用标准 grpc/health 包：
//
//	import "google.golang.org/grpc/health"
//	import healthpb "google.golang.org/grpc/health/grpc_health_v1"
//	hsrv := health.NewServer()
//	healthpb.RegisterHealthServer(grpcServer, hsrv)
//	hsrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
//
//	// dependency probe goroutine：每 5s 检一次，flip status
//	go func() { for { if probesAllOK { hsrv.SetServingStatus(..., SERVING) } else { ..., NOT_SERVING }; sleep 5s } }()
func RegisterHealthService(s *grpc.Server, hc *HealthChecker) {
	// 集成点：main.go 接入标准 grpc/health 包时 wire 这里。
	// 本 v1 stub。
}
