package accounting

import (
	"context"
	"errors"
	"testing"

	accv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"

	"github.com/xiongwp/order-core/internal/domain"
)

// TestEntriesDirection_KnownEvents — 借贷方向是资金链路最敏感的逻辑, 单测覆盖
// 所有已支持的 EventType. 出错会被会计审计直接发现, 但越早测越好.
func TestEntriesDirection_KnownEvents(t *testing.T) {
	const channelNo, ownerNo = "CHAN-001", "OWNR-001"
	cases := []struct {
		event   domain.AccountingEventType
		wantDR  string
		wantCR  string
		wantErr bool
	}{
		// 入账: DEBIT channel, CREDIT owner
		{domain.AccountingEventChargeSucceeded, channelNo, ownerNo, false},
		{domain.AccountingEventDisputeWon, channelNo, ownerNo, false},
		// 出账: DEBIT owner, CREDIT channel
		{domain.AccountingEventRefundSucceeded, ownerNo, channelNo, false},
		{domain.AccountingEventDisputeOpened, ownerNo, channelNo, false},
		{domain.AccountingEventChargebackReceived, ownerNo, channelNo, false},
		{domain.AccountingEventReversalSucceeded, ownerNo, channelNo, false},
		// fee_charged 当前 mapper 不支持 (需要 platform-pnl 账户)
		{domain.AccountingEventFeeCharged, "", "", true},
		// 未识别事件
		{domain.AccountingEventType("totally_made_up"), "", "", true},
	}
	for _, c := range cases {
		t.Run(string(c.event), func(t *testing.T) {
			dr, cr, err := entriesDirection(c.event, channelNo, ownerNo)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for event=%s, got nil (dr=%s cr=%s)", c.event, dr, cr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for event=%s: %v", c.event, err)
			}
			if dr != c.wantDR || cr != c.wantCR {
				t.Errorf("event=%s: got dr=%s cr=%s, want dr=%s cr=%s",
					c.event, dr, cr, c.wantDR, c.wantCR)
			}
		})
	}
}

func TestEventTypeToBusinessType(t *testing.T) {
	cases := []struct {
		event domain.AccountingEventType
		want  accv1.BusinessType
	}{
		{domain.AccountingEventChargeSucceeded, accv1.BusinessType_BUSINESS_TYPE_PAYMENT},
		{domain.AccountingEventDisputeWon, accv1.BusinessType_BUSINESS_TYPE_PAYMENT},
		{domain.AccountingEventRefundSucceeded, accv1.BusinessType_BUSINESS_TYPE_REFUND},
		{domain.AccountingEventDisputeOpened, accv1.BusinessType_BUSINESS_TYPE_REFUND},
		{domain.AccountingEventChargebackReceived, accv1.BusinessType_BUSINESS_TYPE_REFUND},
		{domain.AccountingEventReversalSucceeded, accv1.BusinessType_BUSINESS_TYPE_REFUND},
		{domain.AccountingEventFeeCharged, accv1.BusinessType_BUSINESS_TYPE_COMMISSION},
		// 未识别保守落 PAYMENT, 不阻断.
		{domain.AccountingEventType("future_event"), accv1.BusinessType_BUSINESS_TYPE_PAYMENT},
	}
	for _, c := range cases {
		t.Run(string(c.event), func(t *testing.T) {
			if got := eventTypeToBusinessType(c.event); got != c.want {
				t.Errorf("event=%s: got %v, want %v", c.event, got, c.want)
			}
		})
	}
}

func TestOwnerBTFor(t *testing.T) {
	cases := []struct {
		owner   domain.AccountingOwnerType
		want    accv1.AccountBusinessType
		wantErr bool
	}{
		{domain.AccountingOwnerMerchant, accv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_MERCHANT_PENDING_SETTLE, false},
		{domain.AccountingOwnerUser, accv1.AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE, false},
		{domain.AccountingOwnerType("ghost"), 0, true},
	}
	for _, c := range cases {
		t.Run(string(c.owner), func(t *testing.T) {
			got, err := ownerBTFor(c.owner)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for owner=%s", c.owner)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("owner=%s: got %v, want %v", c.owner, got, c.want)
			}
		})
	}
}

// TestResolveOwnerID 覆盖三种解析路径:
//
//  1. 无 resolver + 纯数字 → strconv.ParseInt
//  2. 无 resolver + 非数字 → error
//  3. resolver 返成功 → 走 resolver
//  4. resolver 返 ErrFallbackToNumeric → fallback 到 strconv
//  5. resolver 返其它 error → propagate
func TestResolveOwnerID(t *testing.T) {
	ctx := context.Background()

	t.Run("no resolver, numeric", func(t *testing.T) {
		c := &Client{}
		id, err := c.resolveOwnerID(ctx, "merchant", "12345")
		if err != nil || id != 12345 {
			t.Fatalf("got id=%d err=%v, want 12345/nil", id, err)
		}
	})

	t.Run("no resolver, non-numeric", func(t *testing.T) {
		c := &Client{}
		_, err := c.resolveOwnerID(ctx, "merchant", "mch_alpha")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("resolver success", func(t *testing.T) {
		c := &Client{}
		c.cfg.OwnerIDResolver = func(_ context.Context, ot, oid string) (int64, error) {
			if ot == "merchant" && oid == "mch_alpha" {
				return 9001, nil
			}
			return 0, errors.New("unknown")
		}
		id, err := c.resolveOwnerID(ctx, "merchant", "mch_alpha")
		if err != nil || id != 9001 {
			t.Fatalf("got id=%d err=%v, want 9001/nil", id, err)
		}
	})

	t.Run("resolver fallback to numeric", func(t *testing.T) {
		c := &Client{}
		c.cfg.OwnerIDResolver = func(_ context.Context, _, _ string) (int64, error) {
			return 0, ErrFallbackToNumeric
		}
		id, err := c.resolveOwnerID(ctx, "merchant", "7777")
		if err != nil || id != 7777 {
			t.Fatalf("got id=%d err=%v, want 7777/nil", id, err)
		}
	})

	t.Run("resolver hard error propagates", func(t *testing.T) {
		c := &Client{}
		boom := errors.New("merchant repo down")
		c.cfg.OwnerIDResolver = func(_ context.Context, _, _ string) (int64, error) {
			return 0, boom
		}
		_, err := c.resolveOwnerID(ctx, "merchant", "irrelevant")
		if !errors.Is(err, boom) {
			t.Fatalf("expected propagated boom error, got %v", err)
		}
	})
}
