// audit-log server — 全局 hash chain 审计日志 — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
//
// 端点:
//
//	POST /api/v1/audit/log               admin svc 写日志
//	POST /api/v1/audit/batch             批量写
//	GET  /api/v1/audit/logs              查询
//	GET  /api/v1/audit/verify            校验整条 chain
//	GET  /api/v1/audit/export?format=csv 合规导出
package main

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"reconcile-system/packages/audit-log/internal/domain"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newMemoryRepo,
			newHTTPServer,
		),
		fx.Invoke(
			startHTTPServer,
			startChainVerifyCron,
		),
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

func newHTTPServer(repo *memoryRepo) *http.Server {
	port := envOr("AUDIT_HTTP_PORT", "8080")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/audit/log", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var e domain.AuditEntry
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			writeErr(w, 400, err)
			return
		}
		// 服务端补字段 — 防客户端伪造
		e.CreatedAt = time.Now().UTC()
		// IP 来自 X-Forwarded-For 或 RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			e.ActorIP = xff
		} else {
			e.ActorIP = r.RemoteAddr
		}
		repo.append(&e)
		writeJSON(w, 201, e)
	})

	// ── batch ingest — 给 payment-util/auditlog 客户端用 ──
	// 单次 POST 多条, 降低 HTTPSink 一条一条 POST 的开销.
	// 限制: 单 batch ≤ 200 条; 整 body ≤ 1MB.
	mux.HandleFunc("/api/v1/audit/batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024*1024) // 1 MB 上限
		var body struct {
			Entries []domain.AuditEntry `json:"entries"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, err)
			return
		}
		if len(body.Entries) == 0 || len(body.Entries) > 200 {
			writeErr(w, 400, fmt.Errorf("entries count must be 1..200, got %d", len(body.Entries)))
			return
		}
		// 服务端补字段 (跟单条 ingest 一致)
		now := time.Now().UTC()
		clientIP := r.RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			clientIP = xff
		}
		for i := range body.Entries {
			body.Entries[i].CreatedAt = now
			if body.Entries[i].ActorIP == "" {
				body.Entries[i].ActorIP = clientIP
			}
		}
		// hash chain 必须序列化 append (并发安全由 repo 内 mutex 保证)
		for i := range body.Entries {
			repo.append(&body.Entries[i])
		}
		writeJSON(w, 201, map[string]any{
			"accepted":     len(body.Entries),
			"first_seq_id": body.Entries[0].SequenceID,
			"last_seq_id":  body.Entries[len(body.Entries)-1].SequenceID,
		})
	})

	mux.HandleFunc("/api/v1/audit/logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filters := map[string]string{
			"service":       q.Get("service"),
			"actor_email":   q.Get("actor_email"),
			"action":        q.Get("action"),
			"resource_type": q.Get("resource_type"),
			"resource_id":   q.Get("resource_id"),
		}
		limit := 100
		if v := q.Get("limit"); v != "" {
			limit, _ = strconv.Atoi(v)
		}
		out := repo.search(filters, limit)
		writeJSON(w, 200, map[string]any{"entries": out, "count": len(out)})
	})

	mux.HandleFunc("/api/v1/audit/verify", func(w http.ResponseWriter, r *http.Request) {
		result := repo.verifyChain()
		writeJSON(w, 200, result)
	})

	mux.HandleFunc("/api/v1/audit/export", func(w http.ResponseWriter, r *http.Request) {
		format := r.URL.Query().Get("format")
		entries := repo.snapshot()
		if format == "csv" {
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition",
				`attachment; filename="audit-`+time.Now().Format("2006-01-02")+`.csv"`)
			cw := csv.NewWriter(w)
			defer cw.Flush()
			cw.Write([]string{"sequence_id", "created_at", "service", "actor_email",
				"actor_ip", "action", "resource_type", "resource_id", "chain_hash", "note"})
			for _, e := range entries {
				cw.Write([]string{
					strconv.FormatInt(e.SequenceID, 10),
					e.CreatedAt.Format(time.RFC3339Nano),
					e.Service, e.ActorEmail, e.ActorIP, e.Action,
					e.ResourceType, e.ResourceID, e.ChainHash, e.Note,
				})
			}
			return
		}
		writeJSON(w, 200, map[string]any{"entries": entries})
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	return &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("audit-log listening", zap.String("addr", srv.Addr))
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

// startChainVerifyCron 定时 chain verify — 每 1h 一次, 发现篡改 critical alert.
func startChainVerifyCron(lc fx.Lifecycle, repo *memoryRepo, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				t := time.NewTicker(1 * time.Hour)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						r := repo.verifyChain()
						if !r.OK {
							log.Error("AUDIT CHAIN TAMPERED",
								zap.Int64("bad_index", r.BadIndex),
								zap.String("reason", r.Reason))
							// 真实场景: 发 PagerDuty + 锁 admin web 写入 + 邮件法务
						}
					}
				}
			}()
			log.Info("audit chain verify cron started")
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

// ─── memory repo + hash chain ───────────────────────────────────────

type memoryRepo struct {
	mu      sync.RWMutex
	entries []*domain.AuditEntry
	nextSeq int64
}

func newMemoryRepo() *memoryRepo { return &memoryRepo{} }

func (m *memoryRepo) append(e *domain.AuditEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSeq++
	e.SequenceID = m.nextSeq
	e.ID = m.nextSeq
	// chain_hash[N] = sha256(chain_hash[N-1] || canonical_json(e))
	prevHash := ""
	if len(m.entries) > 0 {
		prevHash = m.entries[len(m.entries)-1].ChainHash
	}
	e.ChainHash = computeChainHash(prevHash, e)
	m.entries = append(m.entries, e)
}

func computeChainHash(prev string, e *domain.AuditEntry) string {
	// canonical：固定字段顺序 + 不带 chain_hash 字段自身
	canonical := struct {
		Seq      int64  `json:"seq"`
		Svc      string `json:"svc"`
		Actor    string `json:"actor"`
		Action   string `json:"action"`
		ResType  string `json:"rt"`
		ResID    string `json:"rid"`
		Before   string `json:"b"`
		After    string `json:"a"`
		Created  string `json:"ts"`
	}{
		Seq: e.SequenceID, Svc: e.Service, Actor: e.ActorEmail,
		Action: e.Action, ResType: e.ResourceType, ResID: e.ResourceID,
		Before: e.Before, After: e.After,
		Created: e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	body, _ := json.Marshal(canonical)
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (m *memoryRepo) search(filters map[string]string, limit int) []*domain.AuditEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*domain.AuditEntry
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := m.entries[i]
		ok := true
		for k, v := range filters {
			if v == "" {
				continue
			}
			switch k {
			case "service":
				if e.Service != v {
					ok = false
				}
			case "actor_email":
				if e.ActorEmail != v {
					ok = false
				}
			case "action":
				if e.Action != v {
					ok = false
				}
			case "resource_type":
				if e.ResourceType != v {
					ok = false
				}
			case "resource_id":
				if e.ResourceID != v {
					ok = false
				}
			}
			if !ok {
				break
			}
		}
		if !ok {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (m *memoryRepo) snapshot() []*domain.AuditEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.AuditEntry, len(m.entries))
	copy(out, m.entries)
	sort.Slice(out, func(i, j int) bool { return out[i].SequenceID < out[j].SequenceID })
	return out
}

// verifyChain 全量校验：重算每条 hash 比对存的。
func (m *memoryRepo) verifyChain() domain.VerifyResult {
	m.mu.RLock()
	defer m.mu.RUnlock()
	prev := ""
	for i, e := range m.entries {
		expected := computeChainHash(prev, e)
		if expected != e.ChainHash {
			return domain.VerifyResult{
				OK: false, TotalCount: int64(len(m.entries)),
				BadIndex: int64(i),
				Reason: fmt.Sprintf("hash mismatch at seq=%d (expected %s, got %s)",
					e.SequenceID, expected[:16], e.ChainHash[:16]),
			}
		}
		prev = e.ChainHash
	}
	return domain.VerifyResult{OK: true, TotalCount: int64(len(m.entries))}
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
