package handler

import (
	"context"
	"net/http"
	"time"

	kmsv1 "reconcile-system/packages/kms-manage/kitex_gen/kms/v1"
	orderv1 "reconcile-system/packages/order-core/kitex_gen/order/v1"
	usermerchantv1 "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

type DashboardHandler struct{ deps clients.Deps }

func NewDashboardHandler(d clients.Deps) *DashboardHandler { return &DashboardHandler{deps: d} }

// GET /api/dashboard/summary
// 聚合一个首屏卡片用的数字：总订单数、最近订单、KMS 活跃 key 等。
func (h *DashboardHandler) Summary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	out := map[string]interface{}{}

	// 订单数 + 最近
	if piList, err := h.deps.PI.List(ctx, &orderv1.ListPaymentIntentsRequest{Page: 1, PageSize: 5}); err == nil {
		out["orders_total"] = piList.GetTotal()
		recent := make([]map[string]interface{}, 0, len(piList.GetPaymentIntents()))
		for _, pi := range piList.GetPaymentIntents() {
			recent = append(recent, piSummary(pi))
		}
		out["recent_orders"] = recent
	} else {
		out["orders_error"] = err.Error()
	}

	// KMS 状态
	if k, err := h.deps.KMS.ListKeys(ctx, &kmsv1.ListKeysRequest{}); err == nil {
		out["kms_active_key"] = k.GetActiveKeyId()
		out["kms_keys_total"] = len(k.GetKeys())
	} else {
		out["kms_error"] = err.Error()
	}

	// 商户总数 + 按 KYC 分布。两次 List 代价低（都走 RO + cache），List 自带 total。
	if all, err := h.deps.Merchant.List(ctx, &usermerchantv1.ListMerchantsRequest{Limit: 1}); err == nil {
		out["merchants_total"] = all.GetTotal()
	}
	if approved, err := h.deps.Merchant.List(ctx, &usermerchantv1.ListMerchantsRequest{
		KycStatus: usermerchantv1.KycStatus_KYC_STATUS_APPROVED,
		Limit:     1,
	}); err == nil {
		out["merchants_approved"] = approved.GetTotal()
	}
	if pending, err := h.deps.Merchant.List(ctx, &usermerchantv1.ListMerchantsRequest{
		KycStatus: usermerchantv1.KycStatus_KYC_STATUS_PENDING,
		Limit:     1,
	}); err == nil {
		out["merchants_kyc_pending"] = pending.GetTotal()
	}

	// 最近 5 条商户审计（append-only 链），让值班在首屏就看到有没有异常变更。
	if audits, err := h.deps.UserMerchantAudit.List(ctx, &usermerchantv1.ListAuditLogsRequest{Limit: 5}); err == nil {
		out["recent_user_merchant_audits"] = audits.GetEntries()
	}

	writeJSON(w, out)
}
