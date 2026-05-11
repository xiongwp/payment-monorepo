// Package clients — 调 accounting-system 的 adapter。
//
// split-payment 不持账, 不维护账户余额表。所有资金状态以 accounting-system 为准。
// 这里把分账的 N 个 split items 打包成 *一个* AtomicBatchBookingRequest, 让
// accounting 保证原子性: 要么全成功, 要么全回滚, 没有"拆一半"的中间态。

package clients

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"

	accountingv1 "github.com/xiongwp/accounting-system/api/proto/accounting/v1"
)

// AccountingClient gRPC 客户端 + business_type 配置。
type AccountingClient struct {
	c            accountingv1.AccountingServiceClient
	BusinessType accountingv1.BusinessType // 给 split-payment 申请的 enum 值, 如 BUSINESS_TYPE_SPLIT_PAYMENT
	ProductCode  string                    // 见 MoneyFlow product_code (业务侧定义)
	SceneCode    string                    // e.g. "split_marketplace"
	Timeout      time.Duration
}

// NewAccountingClient 构造。
func NewAccountingClient(c accountingv1.AccountingServiceClient) *AccountingClient {
	return &AccountingClient{
		c:            c,
		BusinessType: accountingv1.BusinessType_BUSINESS_TYPE_SPLIT_PAYMENT, // 待 proto 加
		ProductCode:  "split_payment",
		SceneCode:    "split_marketplace",
		Timeout:      5 * time.Second,
	}
}

// PostSplitAtomic 把一次拆分的 N 条记账一次性原子下账。
//
// 一条 split item → 一个 booking entry pair (借: platform_collected, 贷: beneficiary_account)
// 全部打包成 AtomicBatchBookingRequest, 任何一条失败 → 全部回滚, 不会出现"一半成功"。
//
// 返:
//   voucher_no   — accounting 颁发的凭证号 (审计 / 对账查得到)
//   transactionIDs — 每条分录的 tx_id
//   err          — 任何一项失败
func (a *AccountingClient) PostSplitAtomic(ctx context.Context, plan *domain.Plan) (string, []string, error) {
	if plan == nil || len(plan.Items) == 0 {
		return "", nil, errors.New("empty plan")
	}
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	requests := make([]*accountingv1.AtomicBatchBookingEntry, 0, len(plan.Items))
	for _, it := range plan.Items {
		// 一对借贷: 平台未结算池 → 收款方
		entries := []*accountingv1.AccountingEntry{
			{
				AccountId: fmt.Sprintf("platform_unsettled/%s", plan.MerchantID),
				Amount:    strconv.FormatInt(it.AmountMinor, 10),
				Direction: accountingv1.Direction_DIRECTION_DEBIT,
			},
			{
				AccountId: it.Beneficiary,
				Amount:    strconv.FormatInt(it.AmountMinor, 10),
				Direction: accountingv1.Direction_DIRECTION_CREDIT,
			},
		}
		requests = append(requests, &accountingv1.AtomicBatchBookingEntry{
			RequestId:    fmt.Sprintf("split-%d-pos-%d", plan.ID, it.Position),
			BusinessNo:   fmt.Sprintf("split-%d", plan.ID),
			BusinessType: a.BusinessType,
			Entries:      entries,
			Currency:     plan.Currency,
			Description: fmt.Sprintf("split charge=%s pos=%d → %s",
				plan.ChargeID, it.Position, it.Beneficiary),
		})
	}

	resp, err := a.c.AtomicBatchBooking(ctx, &accountingv1.AtomicBatchBookingRequest{
		BatchRequestId:  fmt.Sprintf("split-batch-%d", plan.ID), // 幂等
		BatchBusinessNo: fmt.Sprintf("split-%d", plan.ID),
		Requests:        requests,
		Description:     fmt.Sprintf("split payment for charge %s", plan.ChargeID),
	})
	if err != nil {
		return "", nil, fmt.Errorf("atomic batch booking: %w", err)
	}
	if !resp.AllSuccess {
		// accounting 已经把已成功的部分回滚了 (按 atomic 语义), 这里直接报错
		return "", nil, fmt.Errorf("atomic batch booking partial fail: code=%d msg=%s",
			resp.Code, resp.Message)
	}

	// 收集所有 tx_id
	allTxs := []string{}
	voucherNo := ""
	for _, r := range resp.Results {
		allTxs = append(allTxs, r.TransactionIds...)
		if voucherNo == "" {
			voucherNo = r.VoucherNo
		}
	}
	return voucherNo, allTxs, nil
}

// ReverseSplit 退款时反向 atomic 下账。
// 跟 PostSplitAtomic 对称, 但方向相反 (从 beneficiary 划回 platform_refund_pool)。
func (a *AccountingClient) ReverseSplit(ctx context.Context, rev *domain.Reversal, origPlan *domain.Plan) (string, error) {
	if rev == nil || len(rev.Items) == 0 {
		return "", errors.New("empty reversal")
	}
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	requests := make([]*accountingv1.AtomicBatchBookingEntry, 0, len(rev.Items)*2)
	for i, ri := range rev.Items {
		// 1. 卖家账户 → 平台退款池 (覆盖部分)
		covered := ri.AmountMinor - ri.Shortfall
		if covered > 0 {
			requests = append(requests, &accountingv1.AtomicBatchBookingEntry{
				RequestId:    fmt.Sprintf("rev-%d-%d-from-beneficiary", rev.ID, i),
				BusinessNo:   fmt.Sprintf("rev-%d", rev.ID),
				BusinessType: a.BusinessType,
				Currency:     origPlan.Currency,
				Description:  fmt.Sprintf("reverse split refund=%s ← %s", rev.RefundID, ri.Beneficiary),
				Entries: []*accountingv1.AccountingEntry{
					{
						AccountId: ri.Beneficiary,
						Amount:    strconv.FormatInt(covered, 10),
						Direction: accountingv1.Direction_DIRECTION_DEBIT,
					},
					{
						AccountId: fmt.Sprintf("platform_refund_pool/%s", origPlan.MerchantID),
						Amount:    strconv.FormatInt(covered, 10),
						Direction: accountingv1.Direction_DIRECTION_CREDIT,
					},
				},
			})
		}
		// 2. 平台垫付缺口 (平台应收 → 平台退款池)
		if ri.Shortfall > 0 {
			requests = append(requests, &accountingv1.AtomicBatchBookingEntry{
				RequestId:    fmt.Sprintf("rev-%d-%d-shortfall", rev.ID, i),
				BusinessNo:   fmt.Sprintf("rev-%d", rev.ID),
				BusinessType: a.BusinessType,
				Currency:     origPlan.Currency,
				Description:  fmt.Sprintf("platform covers shortfall for %s ← %s", rev.RefundID, ri.Beneficiary),
				Entries: []*accountingv1.AccountingEntry{
					{
						AccountId: "platform_receivable",
						Amount:    strconv.FormatInt(ri.Shortfall, 10),
						Direction: accountingv1.Direction_DIRECTION_DEBIT,
					},
					{
						AccountId: fmt.Sprintf("platform_refund_pool/%s", origPlan.MerchantID),
						Amount:    strconv.FormatInt(ri.Shortfall, 10),
						Direction: accountingv1.Direction_DIRECTION_CREDIT,
					},
				},
			})
		}
	}

	resp, err := a.c.AtomicBatchBooking(ctx, &accountingv1.AtomicBatchBookingRequest{
		BatchRequestId:  fmt.Sprintf("split-reverse-%d", rev.ID),
		BatchBusinessNo: fmt.Sprintf("rev-%d", rev.ID),
		Requests:        requests,
		Description:     fmt.Sprintf("reverse split for refund %s", rev.RefundID),
	})
	if err != nil {
		return "", err
	}
	if !resp.AllSuccess {
		return "", fmt.Errorf("reverse failed code=%d msg=%s", resp.Code, resp.Message)
	}
	voucher := ""
	if len(resp.Results) > 0 {
		voucher = resp.Results[0].VoucherNo
	}
	return voucher, nil
}

// PostMovements 把 RunPlan 的 N 条 Movement 一次性原子下账。
// 跟 PostSplitAtomic 类似, 但输入是 Graph 派生的通用 Movement (不限于 split, 也支持
// referral / hold release / dispute reversal 等任意资金流)。
func (a *AccountingClient) PostMovements(ctx context.Context, plan *domain.RunPlan) (string, []string, error) {
	if plan == nil || len(plan.Movements) == 0 {
		return "", nil, errors.New("empty plan")
	}
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	// 把 pending 状态的 movement 打包 (skipped 的不入账)
	requests := make([]*accountingv1.AtomicBatchBookingEntry, 0, len(plan.Movements))
	for i, mv := range plan.Movements {
		if mv.Status != "pending" {
			continue
		}
		entries := []*accountingv1.AccountingEntry{
			{
				AccountId: mv.FromAccount,
				Amount:    strconv.FormatInt(mv.AmountMinor, 10),
				Direction: accountingv1.Direction_DIRECTION_DEBIT,
			},
			{
				AccountId: mv.ToAccount,
				Amount:    strconv.FormatInt(mv.AmountMinor, 10),
				Direction: accountingv1.Direction_DIRECTION_CREDIT,
			},
		}
		requests = append(requests, &accountingv1.AtomicBatchBookingEntry{
			RequestId:    fmt.Sprintf("moneyflow-%d-mv-%d", plan.ID, i),
			BusinessNo:   fmt.Sprintf("moneyflow-%d", plan.ID),
			BusinessType: a.BusinessType,
			Entries:      entries,
			Currency:     plan.Currency,
			Description:  fmt.Sprintf("graph=%d.%s edge=%s→%s charge=%s",
				plan.GraphID, plan.GraphVersion, mv.EdgeFromNode, mv.EdgeToNode, plan.ChargeID),
		})
	}
	if len(requests) == 0 {
		// 全 skip
		return "", nil, nil
	}

	resp, err := a.c.AtomicBatchBooking(ctx, &accountingv1.AtomicBatchBookingRequest{
		BatchRequestId:  fmt.Sprintf("moneyflow-batch-%d", plan.ID),
		BatchBusinessNo: fmt.Sprintf("moneyflow-%d", plan.ID),
		Requests:        requests,
		Description: fmt.Sprintf("money flow graph=%d.%s trigger=%s charge=%s",
			plan.GraphID, plan.GraphVersion, plan.TriggerEvent, plan.ChargeID),
	})
	if err != nil {
		return "", nil, fmt.Errorf("atomic batch booking: %w", err)
	}
	if !resp.AllSuccess {
		return "", nil, fmt.Errorf("atomic batch partial fail: code=%d msg=%s", resp.Code, resp.Message)
	}
	voucher := ""
	allTxs := []string{}
	for _, r := range resp.Results {
		allTxs = append(allTxs, r.TransactionIds...)
		if voucher == "" {
			voucher = r.VoucherNo
		}
	}
	return voucher, allTxs, nil
}

// GetBalance 查账户余额 (用于 reverse 时判断够不够扣)。
func (a *AccountingClient) GetBalance(ctx context.Context, accountID, currency string) (int64, error) {
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}
	resp, err := a.c.GetBalanceSnapshot(ctx, &accountingv1.GetBalanceSnapshotRequest{
		AccountId: accountID,
		Currency:  currency,
	})
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(resp.Snapshot.GetBalance(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse balance %q: %w", resp.Snapshot.GetBalance(), err)
	}
	return n, nil
}
