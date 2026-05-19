package handler

import (
	"encoding/json"
	"net/http"

	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	accountingservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"
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

	// RebuildHotAccounts RPC 在 proto 精简时砍掉, 临时降级为 dry_run 报告 0 行.
	_ = req
	writeJSON(w, map[string]interface{}{
		"as_of":   req.AsOf,
		"dry_run": req.DryRun,
		"total":   0,
		"updated": 0,
		"skipped": 0,
		"failed":  0,
		"entries": []map[string]interface{}{},
		"stub":    true,
	})
}
