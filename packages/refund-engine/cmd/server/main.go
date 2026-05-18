// refund-engine server 入口 — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
//
// MVP 用 in-memory repo + log notifier + stub channel.
// 生产换: MySQL repo + HTTP/gRPC client 调通道 + 接 merchant-webhook + 接 billing fee_event.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"github.com/xiongwp/payment-util/obsbootstrap"

	"reconcile-system/packages/refund-engine/internal/clients"
	"reconcile-system/packages/refund-engine/internal/domain"
	"reconcile-system/packages/refund-engine/internal/workflow"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newLogLevel,
			newMemoryRepo,
			newWebhookClient,
			newChannel,
			newWorkflowService,
			newBusinessHTTPServer,
			newAdminHTTPServer,
		),
		fx.Invoke(
			startBusinessHTTPServer,
			startAdminHTTPServer,
			startSubmitCron,
		),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
	).Run()
}

// busServer / adminServer 别名让 fx 区分两个 *http.Server.
type busServer struct{ *http.Server }
type adminServer struct{ Inner *obsbootstrap.AdminServer }

func newLogger(lc fx.Lifecycle) (*zap.Logger, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { _ = logger.Sync(); return nil }})
	return logger, nil
}

func newLogLevel() zap.AtomicLevel {
	return zap.NewAtomicLevelAt(zap.InfoLevel)
}

func newMemoryRepo() *memoryRepo { return newMemRepo() }

func newWebhookClient() *clients.WebhookClient {
	return clients.NewWebhookClient(envOr("MERCHANT_WEBHOOK_URL", "http://merchant-webhook:8080"))
}

func newChannel(log *zap.Logger) stubChannel {
	return stubChannel{log: log}
}

func newWorkflowService(repo *memoryRepo, ch stubChannel, notif *clients.WebhookClient, log *zap.Logger) *workflow.Service {
	return workflow.New(repo, ch, notif, log)
}

func newBusinessHTTPServer(svc *workflow.Service, repo *memoryRepo) busServer {
	port := envOr("REFUND_HTTP_PORT", "8080")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/refunds", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req workflow.RefundRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			rfd, err := svc.Request(r.Context(), req)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusCreated, rfd)
		case http.MethodGet:
			cid := r.URL.Query().Get("charge_id")
			if cid == "" {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("charge_id required"))
				return
			}
			refunds, _ := repo.ListByCharge(r.Context(), cid)
			writeJSON(w, http.StatusOK, map[string]any{"refunds": refunds})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v1/refunds/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/refunds/")
		parts := strings.SplitN(rest, "/", 2)
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid id"))
			return
		}
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		switch action {
		case "":
			rfd, _ := repo.Get(r.Context(), id)
			if rfd == nil {
				writeErr(w, http.StatusNotFound, fmt.Errorf("not found"))
				return
			}
			writeJSON(w, http.StatusOK, rfd)
		case "approve":
			by := r.Header.Get("X-Admin-User")
			if by == "" {
				by = "ops"
			}
			if err := svc.Approve(r.Context(), id, by); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"approved": true})
		case "reject":
			by := r.Header.Get("X-Admin-User")
			if by == "" {
				by = "ops"
			}
			var body struct {
				Reason string `json:"reason"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if err := svc.Reject(r.Context(), id, by, body.Reason); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"rejected": true})
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return busServer{Server: &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}}
}

func newAdminHTTPServer(log *zap.Logger, lvl zap.AtomicLevel) adminServer {
	port := envOr("REFUND_ADMIN_HTTP_PORT", "9099")
	return adminServer{Inner: obsbootstrap.NewAdminServer(obsbootstrap.AdminConfig{
		ServiceName: "refund-engine",
		Port:        port,
		Logger:      log,
		LogLevel:    lvl,
	})}
}

func startBusinessHTTPServer(lc fx.Lifecycle, bs busServer, log *zap.Logger) {
	srv := bs.Server
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("refund-engine listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Error("listen failed", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	})
}

func startAdminHTTPServer(lc fx.Lifecycle, as adminServer, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				if err := as.Inner.Run(ctx); err != nil {
					log.Error("admin http exited", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

// startSubmitCron 每 5s 跑 Submit 推 approved → 通道.
func startSubmitCron(lc fx.Lifecycle, svc *workflow.Service, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				t := time.NewTicker(5 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						rctx, c := context.WithTimeout(ctx, 30*time.Second)
						if n, err := svc.Submit(rctx, 50); err == nil && n > 0 {
							log.Info("refunds submitted", zap.Int("count", n))
						} else if err != nil {
							log.Warn("Submit failed", zap.Error(err))
						}
						c()
					}
				}
			}()
			log.Info("refund submit cron started")
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

// ─── memory repo / stub channel ────────────────────────────────────

type memoryRepo struct {
	mu      sync.RWMutex
	byID    map[int64]*domain.Refund
	byRfID  map[string]*domain.Refund
	byIdemp map[string]*domain.Refund
	next    int64
}

func newMemRepo() *memoryRepo {
	return &memoryRepo{
		byID:    map[int64]*domain.Refund{},
		byRfID:  map[string]*domain.Refund{},
		byIdemp: map[string]*domain.Refund{},
	}
}

func (m *memoryRepo) Create(_ context.Context, r *domain.Refund) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	r.ID = m.next
	m.byID[r.ID] = r
	m.byRfID[r.RefundID] = r
	m.byIdemp[r.IdempotencyKey] = r
	return r.ID, nil
}

func (m *memoryRepo) Get(_ context.Context, id int64) (*domain.Refund, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[id], nil
}

func (m *memoryRepo) GetByRefundID(_ context.Context, rfID string) (*domain.Refund, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byRfID[rfID], nil
}

func (m *memoryRepo) GetByIdempotency(_ context.Context, key string) (*domain.Refund, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byIdemp[key], nil
}

func (m *memoryRepo) UpdateStatus(_ context.Context, id int64, to domain.Status, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.byID[id]
	if r == nil {
		return nil
	}
	r.Status = to
	for k, v := range fields {
		switch k {
		case "approved_by":
			if s, ok := v.(string); ok {
				r.ApprovedBy = s
			}
		case "approved_at":
			if t, ok := v.(*time.Time); ok {
				r.ApprovedAt = t
			}
		case "submitted_at":
			if t, ok := v.(*time.Time); ok {
				r.SubmittedAt = t
			}
		case "completed_at":
			if t, ok := v.(*time.Time); ok {
				r.CompletedAt = t
			}
		case "channel_refund_id":
			if s, ok := v.(string); ok {
				r.ChannelRefundID = s
			}
		case "failure_code":
			if s, ok := v.(string); ok {
				r.FailureCode = s
			}
		case "failure_message":
			if s, ok := v.(string); ok {
				r.FailureMessage = s
			}
		case "updated_at":
			if t, ok := v.(time.Time); ok {
				r.UpdatedAt = t
			}
		}
	}
	return nil
}

func (m *memoryRepo) ListByCharge(_ context.Context, chargeID string) ([]*domain.Refund, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*domain.Refund{}
	for _, r := range m.byID {
		if r.ChargeID == chargeID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memoryRepo) ListByStatus(_ context.Context, st domain.Status, limit int) ([]*domain.Refund, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*domain.Refund{}
	for _, r := range m.byID {
		if r.Status == st {
			out = append(out, r)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *memoryRepo) SumRefundedByCharge(_ context.Context, chargeID string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var sum int64
	for _, r := range m.byID {
		if r.ChargeID == chargeID && (r.Status == domain.StatusCompleted || r.Status == domain.StatusSubmitted) {
			sum += r.AmountMinor
		}
	}
	return sum, nil
}

type stubChannel struct{ log *zap.Logger }

func (s stubChannel) SubmitRefund(_ context.Context, r *domain.Refund) (string, error) {
	s.log.Info("stub channel: submitting refund",
		zap.String("refund_id", r.RefundID), zap.String("method", string(r.Method)))
	return "ch_ref_" + r.RefundID, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
