// Package healthx 提供跨服务一致的 liveness / readiness 探针。
//
// 设计：
//   - **Liveness** = 进程活着（cheap，不查依赖）。K8s livenessProbe 用，失败
//     表示进程死锁需要重启。
//   - **Readiness** = 进程能服务流量（查依赖：DB ping / gRPC 上游 / Redis）。
//     K8s readinessProbe 用，失败表示从负载均衡里摘掉。
//
// 失败的 readiness 必须返回 503 + 失败 probe 详情，运维 / SRE 能直接定位。
//
// 用例：
//
//	mux.HandleFunc("/healthz", healthx.Liveness)
//	mux.HandleFunc("/readyz", healthx.Readiness(
//	    healthx.DBProbe("primary", db),
//	    healthx.GRPCProbe("accounting", accountingConn),
//	))
package healthx

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

// Probe 单个依赖检查。Name 在 readiness JSON 输出 + Prometheus label 里用。
// Check 必须在 ctx 截止时间内返回；典型 budget 200-500ms / probe。
type Probe interface {
	Name() string
	Check(ctx context.Context) error
}

// ProbeFunc 让 caller 用闭包构造 Probe。
type ProbeFunc struct {
	N string
	F func(ctx context.Context) error
}

func (p ProbeFunc) Name() string                       { return p.N }
func (p ProbeFunc) Check(ctx context.Context) error   { return p.F(ctx) }

// 单 probe budget：超时也视作 fail（dep 慢 = 不可用）。
const probeBudget = 500 * time.Millisecond

// 整体 readiness 总预算。比 probe budget 略大，留 marshal/IO 余量。
const readinessTotalBudget = 2 * time.Second

// Liveness 永远 200。挂在 /healthz。**不做任何依赖检查** — 如果加入
// 依赖会导致级联（依赖挂 → liveness 挂 → K8s 重启进程 → 重启没用）。
func Liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// Readiness 返回 HTTP handler，所有 probe 通过 → 200；任一失败 → 503。
// 响应 body 是 JSON：{"status":"ok|fail","probes":{name:{ok:bool,err:string}}}。
//
// 并发执行所有 probe：单 probe 慢不会拖累其它的；总响应时间 ≈ max(probe latency)。
func Readiness(probes ...Probe) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTotalBudget)
		defer cancel()

		type result struct {
			OK  bool   `json:"ok"`
			Err string `json:"err,omitempty"`
		}
		results := make(map[string]result, len(probes))
		ch := make(chan struct {
			name string
			res  result
		}, len(probes))
		for _, p := range probes {
			p := p
			go func() {
				pCtx, pCancel := context.WithTimeout(ctx, probeBudget)
				defer pCancel()
				err := p.Check(pCtx)
				r := result{OK: err == nil}
				if err != nil {
					r.Err = err.Error()
				}
				ch <- struct {
					name string
					res  result
				}{p.Name(), r}
			}()
		}
		ok := true
		for i := 0; i < len(probes); i++ {
			x := <-ch
			results[x.name] = x.res
			if !x.res.OK {
				ok = false
			}
		}

		status := "ok"
		code := http.StatusOK
		if !ok {
			status = "fail"
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": status,
			"probes": results,
		})
	}
}

// ─── Built-in probes ─────────────────────────────────────────────────────────

// DBProbe 用 db.PingContext 检查 SQL 连接。GORM / sqlx 都基于 *sql.DB，
// 都有 .DB() / 类似方法返回底层 *sql.DB。
func DBProbe(name string, db *sql.DB) Probe {
	return ProbeFunc{N: name, F: func(ctx context.Context) error {
		return db.PingContext(ctx)
	}}
}

// GRPCProbe 检查 gRPC ClientConn 状态。Ready / Idle 视为 OK；
// Connecting / TransientFailure / Shutdown 视为失败。
//
// 这是**纯连接状态**检查，不发实际 RPC：cheap、不依赖上游业务可用性。
// 上游业务是否真的健康是上游自己 readiness 的职责。
func GRPCProbe(name string, conn *grpc.ClientConn) Probe {
	return ProbeFunc{N: name, F: func(_ context.Context) error {
		st := conn.GetState()
		switch st {
		case connectivity.Ready, connectivity.Idle:
			return nil
		default:
			return errStateNotReady{state: st}
		}
	}}
}

type errStateNotReady struct {
	state connectivity.State
}

func (e errStateNotReady) Error() string {
	return "grpc client state: " + e.state.String()
}
