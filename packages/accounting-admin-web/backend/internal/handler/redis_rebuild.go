package handler

import (
	"encoding/json"
	"net/http"

	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
)

// RedisRebuildHandler 调 accounting-system gRPC 触发热账户重建。
//
// 走 gRPC 而不是直接代理 admin HTTP，是因为 admin-web → accounting-system 之间
// 默认就是 gRPC 通道，鉴权/超时/可观测性已经统一过；admin HTTP 只留给
// CLI（cmd/tools/redis-rebuild）等运维场景。
type RedisRebuildHandler struct {
	client accountingservice.Client
}

func NewRedisRebuildHandler(client accountingservice.Client) *RedisRebuildHandler {
	return &RedisRebuildHandler{client: client}
}

// Rebuild POST /v1/redis/rebuild
//
// Body:
//
//	{
//	  "as_of":       "5m" | RFC3339 | "" (= now),
//	  "account_nos": ["010100001-001"],
//	  "dry_run":     true
//	}
func (h *RedisRebuildHandler) Rebuild(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AsOf       string   `json:"as_of"`
		AccountNos []string `json:"account_nos"`
		DryRun     bool     `json:"dry_run"`
	}
	// 容忍空 body：用全部默认值（dry_run=false, as_of="" → now, account_nos=空 → 全量）
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}

	resp, err := h.client.RebuildHotAccounts(r.Context(), &accountingv1.RebuildHotAccountsRequest{
		AsOf:       req.AsOf,
		AccountNos: req.AccountNos,
		DryRun:     req.DryRun,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}

	type entryJSON struct {
		AccountNo     string `json:"account_no"`
		BalanceBefore string `json:"balance_before"`
		BalanceAfter  string `json:"balance_after"`
		Source        string `json:"source"`
		JournalCutoff string `json:"journal_cutoff,omitempty"`
		Skipped       bool   `json:"skipped"`
		Reason        string `json:"reason,omitempty"`
	}
	entries := make([]entryJSON, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		entries = append(entries, entryJSON{
			AccountNo:     e.AccountNo,
			BalanceBefore: e.BalanceBefore,
			BalanceAfter:  e.BalanceAfter,
			Source:        e.Source,
			JournalCutoff: e.JournalCutoff,
			Skipped:       e.Skipped,
			Reason:        e.Reason,
		})
	}
	writeJSON(w, map[string]interface{}{
		"as_of":    resp.AsOf,
		"dry_run":  resp.DryRun,
		"total":    resp.Total,
		"updated":  resp.Updated,
		"skipped":  resp.Skipped,
		"failed":   resp.Failed,
		"duration": resp.Duration,
		"entries":  entries,
	})
}
