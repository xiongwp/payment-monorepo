package handler

import (
	"net/http"
	"strings"

	accountingv1 "github.com/xiongwp/accounting-grpc-api/gen/accounting/v1"
)

// AdjustmentHandler handles balance adjustment endpoints.
type AdjustmentHandler struct {
	client accountingv1.AccountingServiceClient
}

func NewAdjustmentHandler(client accountingv1.AccountingServiceClient) *AdjustmentHandler {
	return &AdjustmentHandler{client: client}
}

// AdjustBalance POST /v1/adjustment
//
// fund-safety 必须传 idempotency_key + approval_no（proto 注释里都明文写 "必填"）：
//   - idempotency_key 防运维双击 → 下游 TCC 用同 request_id 二次调用直接返
//     首次结果，不会重复记账
//   - approval_no 是权限审计依据；空 approval_no 的调账无审计追溯，直接拒
//
// 历史 bug：BFF 之前完全不读 idempotency_key，每次调用 RequestId 留空，
// 下游 dedup 失效；approval_no 也只是把 client 传啥透传啥，空值放过 → 调账
// 没审计单号。
func (h *AdjustmentHandler) AdjustBalance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountNo       string `json:"account_no"`
		AdjustmentType  string `json:"adjustment_type"`
		Amount          string `json:"amount"`
		IsIncrease      bool   `json:"is_increase"`
		Reason          string `json:"reason"`
		Operator        string `json:"operator"`
		ApprovalNo      string `json:"approval_no"`
		OffsetAccountNo string `json:"offset_account_no"`
		Currency        string `json:"currency"`
		IdempotencyKey  string `json:"idempotency_key"`
	}
	// 64KiB 上限：调账 payload 都是几十字节字符串，不该超 1KB；64K 给余量。
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	// 必填校验
	if strings.TrimSpace(req.AccountNo) == "" {
		writeError(w, http.StatusBadRequest, "account_no required")
		return
	}
	if strings.TrimSpace(req.ApprovalNo) == "" {
		writeError(w, http.StatusBadRequest, "approval_no required (audit trail; proto doc says 必填)")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason required (audit trail)")
		return
	}
	// idempotency_key：body 优先，body 空时回退 Idempotency-Key header（HTTP 标准）
	idemKey := strings.TrimSpace(req.IdempotencyKey)
	if idemKey == "" {
		idemKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if idemKey == "" {
		writeError(w, http.StatusBadRequest,
			"idempotency_key required (防双击重复调账；前端应在 onClick 时一次性 freeze 一把 UUID)")
		return
	}
	// 长度上限：proto string 在 gRPC 没硬限，但调账 idempotency 习惯 36 字节 UUID；
	// 留 128 字节余量给商户自定义前缀。超长直接拒，防业务侧塞奇怪数据。
	if len(idemKey) > 128 {
		writeError(w, http.StatusBadRequest, "idempotency_key too long (max 128)")
		return
	}

	resp, err := h.client.AdjustBalance(r.Context(), &accountingv1.AdjustBalanceRequest{
		AccountNo:       req.AccountNo,
		AdjustmentType:  req.AdjustmentType,
		Amount:          req.Amount,
		IsIncrease:      req.IsIncrease,
		Reason:          req.Reason,
		Operator:        req.Operator,
		ApprovalNo:      req.ApprovalNo,
		OffsetAccountNo: req.OffsetAccountNo,
		Currency:        req.Currency,
		RequestId:       idemKey, // ← 关键：透传到下游 TCC 防重复记账
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, map[string]interface{}{
		"transaction_id": resp.TransactionId,
		"voucher_no":     resp.VoucherNo,
		"balance_before": resp.BalanceBefore,
		"balance_after":  resp.BalanceAfter,
	})
}
