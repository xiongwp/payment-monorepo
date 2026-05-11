// audit-log server — 全局 hash chain 审计日志。
//
// 端点:
//   POST /api/v1/audit/log               admin svc 写日志
//   GET  /api/v1/audit/logs              查询 (by service/actor/resource/time range)
//   GET  /api/v1/audit/verify            校验整条 chain
//   GET  /api/v1/audit/export?format=csv 合规导出

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
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/audit-log/internal/domain"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := envOr("AUDIT_HTTP_PORT", "8080")

	repo := newMemoryRepo()

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

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 定时 chain verify — 每 1h 一次，发现篡改 critical alert
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
					logger.Error("AUDIT CHAIN TAMPERED",
						zap.Int64("bad_index", r.BadIndex),
						zap.String("reason", r.Reason))
					// 真实场景：发 PagerDuty + 锁 admin web 写入 + 邮件法务
				}
			}
		}
	}()

	logger.Info("audit-log listening", zap.String("addr", srv.Addr))
	go srv.ListenAndServe()
	<-ctx.Done()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
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
