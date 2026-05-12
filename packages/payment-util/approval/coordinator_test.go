package approval

import (
	"context"
	"testing"
	"time"
)

func newC() *Coordinator {
	return New(NewMemStore(), nil, DefaultConfig())
}

func TestRequest_CreatesPending(t *testing.T) {
	c := newC()
	a, err := c.Request(context.Background(), Action{
		Type:      "payout_high_value",
		Resource:  "payout_xxx",
		Requester: "ops_alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StatePending {
		t.Errorf("state = %s, want pending", a.State)
	}
	if a.RequiredApprovals != 2 {
		t.Errorf("default required = %d, want 2", a.RequiredApprovals)
	}
	if a.ID == "" {
		t.Error("id not generated")
	}
}

func TestApprove_AdvancesToReviewingThenApproved(t *testing.T) {
	c := newC()
	a, _ := c.Request(context.Background(), Action{
		Type: "payout", Resource: "p1", Requester: "alice",
	})
	// 1st approve → reviewing
	a, err := c.Approve(context.Background(), a.ID, "bob", "verified")
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StateReviewing {
		t.Errorf("state after 1st approve = %s, want reviewing", a.State)
	}
	// 2nd approve → approved
	a, _ = c.Approve(context.Background(), a.ID, "carol", "")
	if a.State != StateApproved {
		t.Errorf("state after 2nd approve = %s, want approved", a.State)
	}
}

func TestSelfApproval_Rejected(t *testing.T) {
	c := newC()
	a, _ := c.Request(context.Background(), Action{
		Type: "x", Resource: "r1", Requester: "alice",
	})
	_, err := c.Approve(context.Background(), a.ID, "alice", "")
	if err != ErrSelfApproval {
		t.Errorf("expected ErrSelfApproval, got %v", err)
	}
}

func TestDuplicateReviewer_Rejected(t *testing.T) {
	c := newC()
	a, _ := c.Request(context.Background(), Action{
		Type: "x", Resource: "r1", Requester: "alice",
	})
	_, _ = c.Approve(context.Background(), a.ID, "bob", "")
	_, err := c.Approve(context.Background(), a.ID, "bob", "again")
	if err != ErrDuplicateReviewer {
		t.Errorf("expected ErrDuplicateReviewer, got %v", err)
	}
}

func TestReject_IsTerminal(t *testing.T) {
	c := newC()
	a, _ := c.Request(context.Background(), Action{
		Type: "x", Resource: "r1", Requester: "alice",
	})
	_, _ = c.Approve(context.Background(), a.ID, "bob", "")
	a, _ = c.Reject(context.Background(), a.ID, "carol", "nope")
	if a.State != StateRejected {
		t.Errorf("after reject state = %s, want rejected", a.State)
	}
	// 再 approve 应失败
	_, err := c.Approve(context.Background(), a.ID, "dave", "")
	if err != ErrAlreadyDecided {
		t.Errorf("expected ErrAlreadyDecided, got %v", err)
	}
}

func TestRequiredApprovals_Three(t *testing.T) {
	c := newC()
	a, _ := c.Request(context.Background(), Action{
		Type: "kms_rotate", Resource: "kek_main", Requester: "alice",
		RequiredApprovals: 3,
	})
	_, _ = c.Approve(context.Background(), a.ID, "bob", "")
	_, _ = c.Approve(context.Background(), a.ID, "carol", "")
	cur, _ := c.Approve(context.Background(), a.ID, "dave", "")
	if cur.State != StateApproved {
		t.Errorf("with 3 required, after 3 approves state = %s, want approved", cur.State)
	}
}

func TestMarkExecuted_RequiresApproved(t *testing.T) {
	c := newC()
	a, _ := c.Request(context.Background(), Action{
		Type: "x", Resource: "r1", Requester: "alice",
	})
	// 没 approved 不能 execute
	if err := c.MarkExecuted(context.Background(), a.ID, "system"); err != ErrInvalidState {
		t.Errorf("expected ErrInvalidState, got %v", err)
	}
}

func TestCancel_ApproverWindowExpired(t *testing.T) {
	c := New(NewMemStore(), nil, Config{
		DefaultRequiredApprovals: 1,
		DefaultExpireAfter:       1 * time.Hour,
		CancelWindow:             10 * time.Millisecond, // 立即过期窗口
	})
	a, _ := c.Request(context.Background(), Action{
		Type: "x", Resource: "r1", Requester: "alice",
	})
	_, _ = c.Approve(context.Background(), a.ID, "bob", "")
	// 状态应已 approved (RequiredApprovals=1)
	time.Sleep(50 * time.Millisecond)
	// 现在 cancel 窗口过了
	if err := c.Cancel(context.Background(), a.ID, "alice", "changed mind"); err != ErrInvalidState {
		t.Errorf("expected ErrInvalidState (window expired), got %v", err)
	}
}
