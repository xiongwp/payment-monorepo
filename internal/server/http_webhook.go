package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/service"
)

// WebhookHTTPServer 处理 POST /wh/{adapter} —— 从渠道收到的回调。
type WebhookHTTPServer struct {
	svc    *service.WebhookService
	logger *zap.Logger
}

func NewWebhookHTTPServer(svc *service.WebhookService, logger *zap.Logger) *WebhookHTTPServer {
	return &WebhookHTTPServer{svc: svc, logger: logger}
}

func (s *WebhookHTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/wh/", s.handleWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func (s *WebhookHTTPServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	adapter := strings.TrimPrefix(r.URL.Path, "/wh/")
	if adapter == "" || strings.Contains(adapter, "/") {
		http.Error(w, "bad adapter", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := s.svc.Ingest(ctx, adapter, headers, body); err != nil {
		// 把业务语义错误映射成合适的 HTTP status，让上游 LB / 攻击者通过 status
		// 区分"网关瞬时故障"和"被拒"两类。否则一律 500 = 渠道侧自动重试，
		// 加重 DB 负担。
		switch {
		case errors.Is(err, domain.ErrWebhookRateLimited):
			s.logger.Warn("webhook rate limited",
				zap.String("adapter", adapter))
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		case errors.Is(err, domain.ErrWebhookReplay):
			s.logger.Warn("webhook replay rejected",
				zap.String("adapter", adapter))
			http.Error(w, "replay window exceeded", http.StatusBadRequest)
		case errors.Is(err, domain.ErrChannelSignatureFail):
			s.logger.Warn("webhook signature fail",
				zap.String("adapter", adapter))
			http.Error(w, "signature invalid", http.StatusUnauthorized)
		default:
			s.logger.Warn("webhook ingest failed",
				zap.String("adapter", adapter),
				zap.Error(err))
			http.Error(w, "ingest failed", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

// ListenAndServe 启动 webhook HTTP。
func (s *WebhookHTTPServer) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.logger.Info("payment-channel webhook http listening", zap.String("addr", addr))
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	return srv.ListenAndServe()
}
