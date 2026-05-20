// kyc-service server — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
// MVP 全部 in-memory + stub provider.
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

	"reconcile-system/packages/kyc-service/internal/domain"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newMemoryRepo,
			newStubProvider,
			newHTTPServer,
		),
		fx.Invoke(startHTTPServer),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
	).Run()
}

func newLogger(lc fx.Lifecycle) (*zap.Logger, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { _ = logger.Sync(); return nil }})
	return logger, nil
}

func newStubProvider(log *zap.Logger) stubProvider {
	return stubProvider{log: log}
}

func newHTTPServer(repo *memoryRepo, prov stubProvider, logger *zap.Logger) *http.Server {
	port := envOr("KYC_HTTP_PORT", "8080")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/kyc/cases", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var c domain.Case
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				writeErr(w, 400, err)
				return
			}
			now := time.Now().UTC()
			c.CreatedAt = now
			c.UpdatedAt = now
			c.SubmittedAt = now
			c.Status = domain.StatusPendingDocs
			if c.CaseType == "" {
				c.CaseType = domain.CaseInitial
			}
			id := repo.create(&c)
			c.CaseNum = fmt.Sprintf("KYB-%s-%06d", now.Format("2006"), id)
			c.ID = id
			repo.update(id, &c)
			writeJSON(w, 201, c)
		case http.MethodGet:
			mid := r.URL.Query().Get("merchant_id")
			cs := repo.listByMerchant(mid)
			writeJSON(w, 200, map[string]any{"cases": cs})
		}
	})
	mux.HandleFunc("/api/v1/kyc/cases/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/kyc/cases/")
		parts := strings.SplitN(rest, "/", 2)
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			writeErr(w, 400, fmt.Errorf("invalid id"))
			return
		}
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		c := repo.get(id)
		if c == nil {
			writeErr(w, 404, fmt.Errorf("not found"))
			return
		}
		switch action {
		case "":
			writeJSON(w, 200, c)
		case "documents":
			if r.Method == http.MethodPost {
				var d domain.Document
				json.NewDecoder(r.Body).Decode(&d)
				d.CaseID = id
				d.CreatedAt = time.Now().UTC()
				repo.addDoc(id, &d)
				writeJSON(w, 201, d)
			} else {
				writeJSON(w, 200, map[string]any{"documents": repo.listDocs(id)})
			}
		case "submit":
			if !domain.ValidTransition(c.Status, domain.StatusSubmitted) {
				writeErr(w, 400, fmt.Errorf("invalid transition from %s", c.Status))
				return
			}
			c.Status = domain.StatusSubmitted
			c.UpdatedAt = time.Now().UTC()
			repo.update(id, c)
			// 异步触发第三方验证
			go runVerification(repo, prov, id, logger)
			writeJSON(w, 200, c)
		case "approve":
			by := r.Header.Get("X-Reviewer")
			if by == "" {
				by = "ops"
			}
			if !domain.ValidTransition(c.Status, domain.StatusApproved) {
				writeErr(w, 400, fmt.Errorf("invalid transition from %s", c.Status))
				return
			}
			now := time.Now().UTC()
			c.Status = domain.StatusApproved
			c.Reviewer = by
			c.ApprovedAt = &now
			c.UpdatedAt = now
			repo.update(id, c)
			writeJSON(w, 200, c)
		case "reject":
			var body struct{ Reason string `json:"reason"` }
			json.NewDecoder(r.Body).Decode(&body)
			c.Status = domain.StatusRejected
			c.RejectReason = body.Reason
			c.UpdatedAt = time.Now().UTC()
			repo.update(id, c)
			writeJSON(w, 200, c)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	return &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, repo *memoryRepo, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			// 后台 monthly review — 简化版每 6h 扫一次
			go func() {
				t := time.NewTicker(6 * time.Hour)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						n := repo.monitoringRescan()
						if n > 0 {
							log.Info("KYC monitoring rescan", zap.Int("flagged", n))
						}
					}
				}
			}()
			log.Info("kyc-service listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Error("listen failed", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			shutCtx, shc := context.WithTimeout(stopCtx, 10*time.Second)
			defer shc()
			return srv.Shutdown(shutCtx)
		},
	})
}

// runVerification 异步：调第三方 → 跑 PEP/sanctions → 算 risk_score → 更新 case。
func runVerification(repo *memoryRepo, prov verifyProvider, caseID int64, log *zap.Logger) {
	c := repo.get(caseID)
	if c == nil {
		return
	}
	c.Status = domain.StatusInReview
	repo.update(caseID, c)

	// 1. doc verify (mock — 真实调 Sumsub/Veriff API)
	res := prov.VerifyDocs(c)
	repo.addEvent(caseID, "doc_verify", res)

	// 2. PEP/Sanctions check
	pepRes := prov.CheckPEPSanctions(c)
	repo.addEvent(caseID, "pep_sanctions", pepRes)

	// 3. 算 risk_tier
	c.RiskScore = res.Score
	if pepRes.Result == "fail" {
		c.RiskScore = 100
	}
	switch {
	case c.RiskScore >= 90:
		c.RiskTier = domain.TierBlocked
	case c.RiskScore >= 60:
		c.RiskTier = domain.TierHigh
	case c.RiskScore >= 30:
		c.RiskTier = domain.TierMedium
	default:
		c.RiskTier = domain.TierLow
	}

	// 4. low risk 自动 approve；其它进 ops 队列
	if c.RiskTier == domain.TierLow && res.Result == "pass" {
		now := time.Now().UTC()
		c.Status = domain.StatusApproved
		c.Reviewer = "system"
		c.ApprovedAt = &now
		c.UpdatedAt = now
		log.Info("KYC auto-approved",
			zap.Int64("case_id", caseID), zap.Int("risk_score", c.RiskScore))
	}
	repo.update(caseID, c)
}

// ─── stub provider ──────────────────────────────────────────────────

type verifyProvider interface {
	VerifyDocs(c *domain.Case) domain.VerificationEvent
	CheckPEPSanctions(c *domain.Case) domain.VerificationEvent
}

type stubProvider struct{ log *zap.Logger }

func (s stubProvider) VerifyDocs(c *domain.Case) domain.VerificationEvent {
	// 生产换 Sumsub.VerifyApplicant() 等
	score := 10 // 低风险 score
	res := "pass"
	if c.ExpectedGMVMonth > 100_000_000_00 { // 月 100w 美元
		score = 50
	}
	return domain.VerificationEvent{
		CaseID: c.ID, Provider: "stub-sumsub", CheckType: "doc_verify",
		Result: res, Score: score,
		CreatedAt: time.Now().UTC(),
	}
}

func (s stubProvider) CheckPEPSanctions(c *domain.Case) domain.VerificationEvent {
	// 生产换真 PEP/sanctions DB (Refinitiv World-Check / Dow Jones)
	res := "pass"
	score := 0
	if strings.Contains(strings.ToLower(c.BusinessCountry), "ir") ||
		strings.Contains(strings.ToLower(c.BusinessCountry), "kp") {
		res = "fail"
		score = 100
	}
	return domain.VerificationEvent{
		CaseID: c.ID, Provider: "stub-worldcheck", CheckType: "pep_sanctions",
		Result: res, Score: score,
		CreatedAt: time.Now().UTC(),
	}
}

// ─── memory repo ────────────────────────────────────────────────────

type memoryRepo struct {
	mu     sync.RWMutex
	cases  map[int64]*domain.Case
	docs   map[int64][]*domain.Document
	events map[int64][]*domain.VerificationEvent
	next   int64
}

func newMemoryRepo() *memoryRepo {
	return &memoryRepo{
		cases:  map[int64]*domain.Case{},
		docs:   map[int64][]*domain.Document{},
		events: map[int64][]*domain.VerificationEvent{},
	}
}

func (m *memoryRepo) create(c *domain.Case) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	c.ID = m.next
	m.cases[m.next] = c
	return m.next
}

func (m *memoryRepo) update(id int64, c *domain.Case) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cases[id] = c
}

func (m *memoryRepo) get(id int64) *domain.Case {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cases[id]
}

func (m *memoryRepo) listByMerchant(mid string) []*domain.Case {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*domain.Case{}
	for _, c := range m.cases {
		if mid == "" || c.MerchantID == mid {
			out = append(out, c)
		}
	}
	return out
}

func (m *memoryRepo) addDoc(caseID int64, d *domain.Document) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d.ID = int64(len(m.docs[caseID])) + 1
	m.docs[caseID] = append(m.docs[caseID], d)
}

func (m *memoryRepo) listDocs(caseID int64) []*domain.Document {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.docs[caseID]
}

func (m *memoryRepo) addEvent(caseID int64, _ string, e domain.VerificationEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = int64(len(m.events[caseID])) + 1
	m.events[caseID] = append(m.events[caseID], &e)
}

// monitoringRescan: 简化 — 每 6h 重新算 risk_tier，看有没有从 low 变 high。
func (m *memoryRepo) monitoringRescan() int {
	// 真实做法：拉新 PEP/sanctions list 比对所有 active 商户
	return 0
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
