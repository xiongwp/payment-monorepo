// Package orchestrator — DSAR/RTBF 跨服务编排.
//
// fan-out 到 ServiceRegistry 里所有支持当前 subject_type + request_type 的服务,
// 调它们的 /internal/data-rights/{export|erase} endpoint, 收集结果回写 ServiceStatus.
//
// 每服务的端点约定:
//
//   POST /internal/data-rights/export
//   {
//     "request_id": "dsar_...",
//     "subject_type": "merchant",
//     "subject_id": "m_123"
//   }
//   ←
//   {
//     "found": true,
//     "size_bytes": 4096,
//     "sha256": "...",
//     "export_url": "s3://...",      // 大文件; 或
//     "data": { ... }                // 小数据直接内联
//   }
//
//   POST /internal/data-rights/erase
//   ←
//   {
//     "erased": true,
//     "partial": false,
//     "held_fields": [],             // 法律保留期内不能删的字段
//     "hold_reason": ""
//   }

package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/data-rights/internal/domain"
	"reconcile-system/packages/data-rights/internal/store"
)

type Orchestrator struct {
	Store    store.Store
	Registry domain.ServiceRegistry
	HTTP     *http.Client
	Log      *zap.Logger
}

func New(s store.Store, reg domain.ServiceRegistry, log *zap.Logger) *Orchestrator {
	if log == nil {
		log = zap.NewNop()
	}
	return &Orchestrator{
		Store:    s,
		Registry: reg,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Log:      log,
	}
}

// CollectAccess fan-out 到所有支持 access 的服务, 并发拉取.
func (o *Orchestrator) CollectAccess(ctx context.Context, req domain.Request) error {
	return o.fanOut(ctx, req, "export", func(s domain.ServiceEntry) bool { return s.SupportsAccess })
}

// CollectErasure fan-out 删除.
func (o *Orchestrator) CollectErasure(ctx context.Context, req domain.Request) error {
	return o.fanOut(ctx, req, "erase", func(s domain.ServiceEntry) bool { return s.SupportsErasure })
}

func (o *Orchestrator) fanOut(ctx context.Context, req domain.Request, action string,
	pred func(domain.ServiceEntry) bool) error {

	// 选 supports 这个 subject_type 的服务
	targets := []domain.ServiceEntry{}
	for _, svc := range o.Registry.Services {
		if !pred(svc) {
			continue
		}
		if len(svc.SubjectTypes) > 0 && !containsSubject(svc.SubjectTypes, req.Subject.Type) {
			continue
		}
		targets = append(targets, svc)
	}

	if len(targets) == 0 {
		o.Log.Warn("no service in registry supports this request",
			zap.String("request_id", req.RequestID),
			zap.String("action", action))
		return fmt.Errorf("no service available")
	}

	var wg sync.WaitGroup
	wg.Add(len(targets))

	for _, target := range targets {
		go func(t domain.ServiceEntry) {
			defer wg.Done()
			o.callOne(ctx, req, t, action)
		}(target)
	}
	wg.Wait()
	return nil
}

func (o *Orchestrator) callOne(ctx context.Context, req domain.Request, target domain.ServiceEntry, action string) {
	endpoint := target.BaseURL + "/internal/data-rights/" + action
	body, _ := json.Marshal(map[string]any{
		"request_id":   req.RequestID,
		"subject_type": req.Subject.Type,
		"subject_id":   req.Subject.ID,
	})
	status := domain.ServiceStatus{
		Service:       target.Name,
		Endpoint:      endpoint,
		Attempts:      1,
		LastAttemptAt: time.Now().UTC(),
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		status.State = domain.StateFailed
		status.Error = err.Error()
		_ = o.Store.UpdateServiceStatus(req.RequestID, status)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if target.AuthHeader != "" {
		httpReq.Header.Set("X-Service-Token", target.AuthHeader)
	}
	httpReq.Header.Set("X-Request-Id", req.RequestID)

	resp, err := o.HTTP.Do(httpReq)
	if err != nil {
		status.State = domain.StateFailed
		status.Error = err.Error()
		_ = o.Store.UpdateServiceStatus(req.RequestID, status)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// 该服务没有这个 subject 的数据 — 不算失败
		status.State = domain.StateFulfilled
		status.Error = "no data"
		_ = o.Store.UpdateServiceStatus(req.RequestID, status)
		return
	}
	if resp.StatusCode >= 300 {
		status.State = domain.StateFailed
		status.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		_ = o.Store.UpdateServiceStatus(req.RequestID, status)
		return
	}

	var out struct {
		Found      bool     `json:"found"`
		Erased     bool     `json:"erased"`
		Partial    bool     `json:"partial"`
		SizeBytes  int64    `json:"size_bytes"`
		SHA256     string   `json:"sha256"`
		ExportURL  string   `json:"export_url"`
		HeldFields []string `json:"held_fields"`
		HoldReason string   `json:"hold_reason"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	status.State = domain.StateFulfilled
	status.ExportSize = out.SizeBytes
	status.ExportSHA = out.SHA256
	if out.Partial {
		status.Held = true
		status.HoldReason = out.HoldReason
	}
	_ = o.Store.UpdateServiceStatus(req.RequestID, status)
}

// CombineExports 把全部服务的 sha256 算总 sha — 给最终交付 zip 文件做完整性证明.
func CombineExports(statuses []domain.ServiceStatus) string {
	h := sha256.New()
	for _, s := range statuses {
		fmt.Fprintf(h, "%s|%s|%d|%s\n", s.Service, s.ExportSHA, s.ExportSize, s.State)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func containsSubject(list []domain.SubjectType, t domain.SubjectType) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}
