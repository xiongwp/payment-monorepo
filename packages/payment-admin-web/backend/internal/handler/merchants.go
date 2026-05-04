package handler

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

// MerchantHandler exposes merchant onboarding + KYC to the admin UI.
// It's a thin BFF over the order-core MerchantService gRPC — no business
// logic added here so the admin can be rebuilt independently of order-core.
type MerchantHandler struct{ deps clients.Deps }

// NewMerchantHandler constructs the handler.
func NewMerchantHandler(d clients.Deps) *MerchantHandler { return &MerchantHandler{deps: d} }

// ─── HTTP routes ─────────────────────────────────────────────────────────────
//
//   GET    /api/merchants                list (status/kyc filters)
//   POST   /api/merchants                create
//   GET    /api/merchants/{id}           get
//   PATCH  /api/merchants/{id}           update whitelisted fields
//   POST   /api/merchants/{id}/rotate-key  {"kind":"live"|"test"}
//   POST   /api/merchants/{id}/kyc/{action}  {actor, reason}
//       action ∈ submit / review / approve / reject / request_more_info /
//                suspend / unsuspend / terminate
//   POST   /api/merchants/{id}/documents           add doc
//   GET    /api/merchants/{id}/documents           list docs
//   POST   /api/merchants/documents/{doc_id}/review {status, note}
//   GET    /api/merchants/{id}/audits              list audits

func (h *MerchantHandler) List(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Merchant.List(ctx, &usermerchantv1.ListMerchantsRequest{
		Status:    parseMerchantStatus(r.URL.Query().Get("status")),
		KycStatus: parseKycStatus(r.URL.Query().Get("kyc_status")),
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"items": resp.GetMerchants(), "total": resp.GetTotal()})
}

func (h *MerchantHandler) Create(w http.ResponseWriter, r *http.Request) {
	var body usermerchantv1.CreateMerchantRequest
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Merchant.Create(ctx, &body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	// Show secrets exactly once; caller MUST persist them.
	writeJSON(w, map[string]any{
		"merchant":        resp.GetMerchant(),
		"live_secret_key": resp.GetLiveSecretKey(),
		"test_secret_key": resp.GetTestSecretKey(),
		"webhook_secret":  resp.GetWebhookSecret(),
		"_warning":        "Secrets above are shown only once. Store them in your vault.",
	})
}

func (h *MerchantHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Merchant.Get(ctx, &usermerchantv1.GetMerchantRequest{Id: id})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetMerchant())
}

func (h *MerchantHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var fields map[string]string
	if err := readJSON(r, &fields); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Merchant.Update(ctx, &usermerchantv1.UpdateMerchantRequest{Id: id, Fields: fields})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetMerchant())
}

func (h *MerchantHandler) RotateAPIKey(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var body struct {
		Kind           string `json:"kind"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Kind != "live" && body.Kind != "test" {
		writeError(w, http.StatusBadRequest, "kind must be 'live' or 'test'")
		return
	}
	// 资损/运维安全：rotate-key 是「one-shot 显示明文」操作，双击会签发两把
	// key 但 UI 只能展示第二把 → 第一把已经在 backend 落库且立即生效但 ops
	// 没记下来 → 商户调用旧 key 全部 401，得再跑一轮 rotate 才能恢复。
	// 用 idempotencyCache 把 5 分钟内同 key 的二次提交映射回首次响应。
	// 参见 idempotency.go 注释：rotate-key 本来就在 high-risk 列表里但之前漏接。
	idemKey := body.IdempotencyKey
	if idemKey == "" {
		idemKey = r.Header.Get("Idempotency-Key")
	}
	cacheKey := "merchant:rotate-key:" + id + ":" + body.Kind + ":" + idemKey
	if idemKey != "" {
		if cached, ok := idempotencyCache.Lookup(cacheKey); ok {
			writeJSON(w, cached)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Merchant.RotateApiKey(ctx, &usermerchantv1.RotateApiKeyRequest{Id: id, Kind: body.Kind})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := map[string]any{
		"plaintext": resp.GetPlaintext(),
		"_warning":  "This key is shown only once. Rotate again if you lose it.",
	}
	if idemKey != "" {
		idempotencyCache.Store(cacheKey, out)
	}
	writeJSON(w, out)
}

// KYCTransition routes POST /api/merchants/{id}/kyc/{action}.
func (h *MerchantHandler) KYCTransition(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, action := vars["id"], vars["action"]
	var body struct {
		Actor          string `json:"actor"`
		Reason         string `json:"reason"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	_ = readJSON(r, &body) // actor/reason optional for some actions
	// 双击防护：approve / reject / terminate 等动作虽对状态机幂等（已 terminated
	// 就不会再变），但下游 user-merchant-core 每次都会写一条 audit log + 触发
	// merchant-side 通知（邮件/webhook）。双击 → 商户收两封"账户已被终止"邮件，
	// 客服很尴尬。简单 BFF 缓存 + cache key 含 action 即可消除大多数 race。
	idemKey := body.IdempotencyKey
	if idemKey == "" {
		idemKey = r.Header.Get("Idempotency-Key")
	}
	cacheKey := "merchant:kyc:" + id + ":" + action + ":" + idemKey
	if idemKey != "" {
		if cached, ok := idempotencyCache.Lookup(cacheKey); ok {
			writeJSON(w, cached)
			return
		}
	}
	req := &usermerchantv1.KycTransitionRequest{Id: id, Actor: body.Actor, Reason: body.Reason}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var (
		resp *usermerchantv1.KycTransitionResponse
		err  error
	)
	switch action {
	case "submit":
		resp, err = h.deps.Merchant.SubmitKyc(ctx, req)
	case "review":
		resp, err = h.deps.Merchant.StartReview(ctx, req)
	case "approve":
		resp, err = h.deps.Merchant.Approve(ctx, req)
	case "reject":
		resp, err = h.deps.Merchant.Reject(ctx, req)
	case "request_more_info":
		resp, err = h.deps.Merchant.RequestMoreInfo(ctx, req)
	case "suspend":
		resp, err = h.deps.Merchant.Suspend(ctx, req)
	case "unsuspend":
		resp, err = h.deps.Merchant.Unsuspend(ctx, req)
	case "terminate":
		resp, err = h.deps.Merchant.Terminate(ctx, req)
	default:
		writeError(w, http.StatusBadRequest, "unknown kyc action: "+action)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := resp.GetMerchant()
	if idemKey != "" {
		idempotencyCache.Store(cacheKey, out)
	}
	writeJSON(w, out)
}

func (h *MerchantHandler) ListDocuments(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Merchant.ListDocuments(ctx, &usermerchantv1.ListKycDocumentsRequest{MerchantId: id})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetDocuments())
}

func (h *MerchantHandler) AddDocument(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var body usermerchantv1.AddKycDocumentRequest
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.MerchantId = id
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Merchant.AddDocument(ctx, &body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetDocument())
}

func (h *MerchantHandler) ReviewDocument(w http.ResponseWriter, r *http.Request) {
	docID := mux.Vars(r)["doc_id"]
	var body struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := h.deps.Merchant.ReviewDocument(ctx, &usermerchantv1.ReviewKycDocumentRequest{
		Id: docID, Status: body.Status, Note: body.Note,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *MerchantHandler) ListAudits(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Merchant.ListAudits(ctx, &usermerchantv1.ListKycAuditsRequest{
		MerchantId: id, Limit: int32(limit),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetAudits())
}

// ─── enum parsers ────────────────────────────────────────────────────────────

func parseMerchantStatus(s string) usermerchantv1.MerchantStatus {
	switch s {
	case "pending":
		return usermerchantv1.MerchantStatus_MERCHANT_STATUS_PENDING
	case "active":
		return usermerchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE
	case "suspended":
		return usermerchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED
	case "terminated":
		return usermerchantv1.MerchantStatus_MERCHANT_STATUS_TERMINATED
	}
	return usermerchantv1.MerchantStatus_MERCHANT_STATUS_UNSPECIFIED
}

func parseKycStatus(s string) usermerchantv1.KycStatus {
	switch s {
	case "pending":
		return usermerchantv1.KycStatus_KYC_STATUS_PENDING
	case "submitted":
		return usermerchantv1.KycStatus_KYC_STATUS_SUBMITTED
	case "reviewing":
		return usermerchantv1.KycStatus_KYC_STATUS_REVIEWING
	case "needs_more_info":
		return usermerchantv1.KycStatus_KYC_STATUS_NEEDS_MORE_INFO
	case "approved":
		return usermerchantv1.KycStatus_KYC_STATUS_APPROVED
	case "rejected":
		return usermerchantv1.KycStatus_KYC_STATUS_REJECTED
	case "suspended":
		return usermerchantv1.KycStatus_KYC_STATUS_SUSPENDED
	case "terminated":
		return usermerchantv1.KycStatus_KYC_STATUS_TERMINATED
	}
	return usermerchantv1.KycStatus_KYC_STATUS_UNSPECIFIED
}
