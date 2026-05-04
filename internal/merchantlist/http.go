package merchantlist

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// RegisterHandlers admin / 商户 API 端点。tenant 隔离规则：
//
//   - 商户 key 调用 → merchant_id 强制 = principal.MerchantID（无视 query 参数）
//   - admin / internal key → query.merchant_id 必填，谁的就查谁的
//
// 端点：
//
//	GET  /admin/merchant_list?merchant_id=&kind=allow|block
//	POST /admin/merchant_list                               Body: Entry
//	DELETE /admin/merchant_list/{kind}/{dim}/{value}?merchant_id=
//
// 注意：本 handler 不直接做 tenant 校验（依赖外层 metrics.AdminAuth /
// metrics.pathScopedAuth + 调用方在 wrapper 里把 principal 注入 ctx）。
// 当前简化为：merchant_id 由 query 参数显式提供；上层 wrapper（main.go 里的
// sessionReg 闭包）应该按 principal scope 决定是否覆盖 query.merchant_id。
type AuthFilter func(r *http.Request, requestedMerchantID string) string

// RegisterHandlers 注册全部 merchant_list 端点。
//   - filter 可传 nil（dev / 完全开放）；生产建议传一个解析 ctx principal
//     的实现，把商户 key 的 query.merchant_id 强制覆盖成 principal.MerchantID。
func RegisterHandlers(mux *http.ServeMux, svc Service, logger *zap.Logger, filter AuthFilter) {
	mux.HandleFunc("/admin/merchant_list", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleList(w, r, svc, filter)
		case http.MethodPost:
			handleAdd(w, r, svc, logger, filter)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/admin/merchant_list/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		handleDelete(w, r, svc, logger, filter)
	})
}

func resolveMerchant(r *http.Request, filter AuthFilter) string {
	mid := strings.TrimSpace(r.URL.Query().Get("merchant_id"))
	if filter != nil {
		mid = filter(r, mid)
	}
	return mid
}

func handleList(w http.ResponseWriter, r *http.Request, svc Service, filter AuthFilter) {
	mid := resolveMerchant(r, filter)
	if mid == "" {
		http.Error(w, `{"error":"merchant_id required"}`, http.StatusBadRequest)
		return
	}
	kind := Kind(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind"))))
	if kind != "" && kind != KindAllow && kind != KindBlock {
		http.Error(w, `{"error":"kind must be allow|block"}`, http.StatusBadRequest)
		return
	}
	entries := svc.List(r.Context(), mid, kind)
	writeJSON(w, http.StatusOK, entries)
}

func handleAdd(w http.ResponseWriter, r *http.Request, svc Service, logger *zap.Logger, filter AuthFilter) {
	var e Entry
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&e); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	// 如果调用方是商户，强制覆盖 e.MerchantID = principal.MerchantID（防止商户
	// 假冒别人写入）。filter 实现里做这件事；这里简化为：filter 决定 mid 后覆盖。
	if filter != nil {
		mid := filter(r, e.MerchantID)
		if mid != "" {
			e.MerchantID = mid
		}
	}
	if e.ExpiresAt.IsZero() && r.URL.Query().Get("ttl") != "" {
		// ?ttl=720h 等 query 参数支持快速设置过期
		if d, err := time.ParseDuration(r.URL.Query().Get("ttl")); err == nil {
			e.ExpiresAt = time.Now().Add(d).UTC()
		}
	}
	if err := svc.Add(r.Context(), e); err != nil {
		logger.Warn("merchant_list add failed", zap.Error(err))
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleDelete(w http.ResponseWriter, r *http.Request, svc Service, logger *zap.Logger, filter AuthFilter) {
	var body struct {
		MerchantID string `json:"merchant_id"`
		Kind       Kind   `json:"kind"`
		Dimension  string `json:"dimension"`
		Value      string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if filter != nil {
		body.MerchantID = filter(r, body.MerchantID)
	}
	if body.MerchantID == "" || body.Kind == "" || body.Dimension == "" || body.Value == "" {
		http.Error(w, `{"error":"merchant_id+kind+dimension+value required"}`, http.StatusBadRequest)
		return
	}
	ok := svc.Remove(r.Context(), body.MerchantID, body.Kind, body.Dimension, body.Value)
	logger.Info("merchant_list delete",
		zap.String("merchant_id", body.MerchantID), zap.String("kind", string(body.Kind)),
		zap.Bool("removed", ok))
	writeJSON(w, http.StatusOK, map[string]bool{"removed": ok})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
