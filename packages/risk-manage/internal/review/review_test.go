package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func mkItem(id string, status Status) Item {
	return Item{ID: id, MerchantID: "m", CustomerID: "c", Amount: 100, Currency: "PHP", Status: status}
}

func TestMemStore_PushIdempotent(t *testing.T) {
	s := NewMemStore()
	if err := s.Push(mkItem("a", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Push(mkItem("a", "")); err != nil {
		t.Fatal(err)
	}
	if got := s.List(StatusPending, 0, 0); len(got) != 1 {
		t.Fatalf("expected 1 item after 2 pushes, got %d", len(got))
	}
}

func TestMemStore_DecideApprove(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	it, err := s.Decide("a", ActionApprove, "ops@example", "looks legit")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != StatusApproved || it.DecidedBy != "ops@example" {
		t.Fatalf("unexpected: %+v", it)
	}
	if it.DecidedAt == nil {
		t.Fatal("DecidedAt should be set")
	}
}

func TestMemStore_DecideReject(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("b", ""))
	it, err := s.Decide("b", ActionReject, "ops@example", "card velocity")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != StatusRejected {
		t.Fatalf("expected rejected, got %v", it.Status)
	}
}

func TestMemStore_DecideTwiceFails(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	if _, err := s.Decide("a", ActionApprove, "u1", ""); err != nil {
		t.Fatal(err)
	}
	_, err := s.Decide("a", ActionReject, "u2", "")
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("expected ErrNotPending on second decide, got %v", err)
	}
}

func TestMemStore_DecideUnknownFails(t *testing.T) {
	s := NewMemStore()
	if _, err := s.Decide("nope", ActionApprove, "u", ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("expected ErrNotPending, got %v", err)
	}
}

func TestMemStore_ListFilter(t *testing.T) {
	s := NewMemStore()
	for _, id := range []string{"a", "b", "c"} {
		s.Push(mkItem(id, ""))
	}
	s.Decide("a", ActionApprove, "u", "")
	s.Decide("b", ActionReject, "u", "")
	if got := s.List(StatusPending, 0, 0); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("pending should be only c; got %+v", got)
	}
	if got := s.List(StatusApproved, 0, 0); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("approved should be a; got %+v", got)
	}
	if got := s.List("", 0, 0); len(got) != 3 {
		t.Fatalf("no filter: expected 3, got %d", len(got))
	}
}

func TestMemStore_PaginationAndOrder(t *testing.T) {
	s := NewMemStore()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		s.Push(mkItem(id, ""))
	}
	page1 := s.List("", 2, 0)
	page2 := s.List("", 2, 2)
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("page sizes: %d / %d", len(page1), len(page2))
	}
	// 最新在前；后写入的（"e"）在第一页
	if page1[0].ID != "e" {
		t.Fatalf("expected newest first; got %s", page1[0].ID)
	}
}

// ─── Case workflow ────────────────────────────────────────────────────────

func TestMemStore_ClaimAndDecide(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	cl, err := s.Claim("a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if cl.Status != StatusInReview || cl.AssignedTo != "alice" || cl.AssignedAt == nil {
		t.Fatalf("claim should set in_review + assignee; got %+v", cl)
	}
	// 同 actor 重复 claim 幂等
	if _, err := s.Claim("a", "alice"); err != nil {
		t.Fatalf("re-claim same actor should be idempotent; got %v", err)
	}
	// 不同 actor 抢 → ErrAlreadyClaimed
	if _, err := s.Claim("a", "bob"); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("expected ErrAlreadyClaimed, got %v", err)
	}
	// alice decide 走 in_review → approved
	dec, err := s.Decide("a", ActionApprove, "alice", "looks good")
	if err != nil || dec.Status != StatusApproved {
		t.Fatalf("decide from in_review should work; %+v / %v", dec, err)
	}
}

func TestMemStore_Release(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	if _, err := s.Claim("a", "alice"); err != nil {
		t.Fatal(err)
	}
	// bob 不能 release alice 的
	if _, err := s.Release("a", "bob"); !errors.Is(err, ErrNotAssignee) {
		t.Fatalf("non-assignee release should fail; got %v", err)
	}
	rel, err := s.Release("a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Status != StatusPending || rel.AssignedTo != "" {
		t.Fatalf("release should clear assignee + back to pending; got %+v", rel)
	}
}

func TestMemStore_Escalate(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	s.Claim("a", "alice")
	es, err := s.Escalate("a", "alice", "complex case")
	if err != nil {
		t.Fatal(err)
	}
	if es.Status != StatusEscalated || es.EscalateLevel != 1 || es.AssignedTo != "" {
		t.Fatalf("escalate state wrong: %+v", es)
	}
	if len(es.Notes) != 1 {
		t.Fatalf("escalate should add 1 note; got %d", len(es.Notes))
	}
	// 资深 senior 来 claim escalated — 当前实现不让 (Status != Pending → 拒)。
	// 等真正多级队列时再放开；此处确认行为：
	if _, err := s.Claim("a", "senior"); err == nil {
		t.Fatalf("escalated case shouldn't be re-claimable via Claim() yet")
	}
	// 但 senior 可以直接 Decide
	dec, err := s.Decide("a", ActionReject, "senior", "fraud confirmed")
	if err != nil || dec.Status != StatusRejected {
		t.Fatalf("senior decide on escalated should work; %+v / %v", dec, err)
	}
}

func TestMemStore_AddNote(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	if _, err := s.AddNote("a", "alice", "called merchant, awaiting reply"); err != nil {
		t.Fatal(err)
	}
	it := s.Get("a")
	if len(it.Notes) != 1 || it.Notes[0].Body != "called merchant, awaiting reply" {
		t.Fatalf("note not recorded: %+v", it.Notes)
	}
	// 已 decided 也能加 note（事后调查）
	s.Decide("a", ActionApprove, "alice", "ok")
	if _, err := s.AddNote("a", "alice", "post-hoc review"); err != nil {
		t.Fatalf("post-hoc note should work: %v", err)
	}
}

func TestMemStore_ListByAssignee(t *testing.T) {
	s := NewMemStore()
	for _, id := range []string{"a", "b", "c"} {
		s.Push(mkItem(id, ""))
	}
	s.Claim("a", "alice")
	s.Claim("b", "alice")
	s.Claim("c", "bob")
	got := s.ListByAssignee("alice", []Status{StatusInReview}, 0)
	if len(got) != 2 {
		t.Fatalf("alice should have 2 cases, got %d", len(got))
	}
	got = s.ListByAssignee("bob", nil, 0)
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("bob should have only c, got %+v", got)
	}
}

func TestMemStore_OverdueSLA(t *testing.T) {
	s := NewMemStore()
	s.SetDefaultSLA(50 * time.Millisecond)
	s.Push(mkItem("a", ""))
	s.Push(mkItem("b", ""))
	// 等过期
	time.Sleep(60 * time.Millisecond)
	overdue := s.OverdueSLA(time.Now(), 0)
	if len(overdue) != 2 {
		t.Fatalf("both should be overdue, got %d", len(overdue))
	}
	// decided 的不算
	s.Decide("a", ActionApprove, "alice", "ok")
	overdue = s.OverdueSLA(time.Now(), 0)
	if len(overdue) != 1 || overdue[0].ID != "b" {
		t.Fatalf("only b should be overdue post-decide, got %+v", overdue)
	}
}

// TestMemStore_OldestPendingAge 覆盖 4 个 case：
//   1. 空队列 -> 0
//   2. 全是 decided -> 0（pending 为空）
//   3. 多个 pending，挑 CreatedAt 最早的
//   4. 已 Decide 不参与（即使 CreatedAt 最早）
func TestMemStore_OldestPendingAge(t *testing.T) {
	now := time.Now().UTC()
	s := NewMemStore()

	// (1) 空队列
	if d := s.OldestPendingAge(now); d != 0 {
		t.Fatalf("empty queue should return 0, got %v", d)
	}

	// 加 3 条 pending，时间分别 -10min / -5min / -1min
	push := func(id string, ago time.Duration, status Status) {
		it := mkItem(id, status)
		it.CreatedAt = now.Add(-ago)
		_ = s.Push(it)
	}
	push("oldest", 10*time.Minute, "")
	push("middle", 5*time.Minute, "")
	push("newest", 1*time.Minute, "")

	d := s.OldestPendingAge(now)
	if d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("expected ~10min, got %v", d)
	}

	// (4) 把 oldest decide 掉 → 最早 pending 应该是 middle (5min)
	if _, err := s.Decide("oldest", ActionApprove, "ops", "ok"); err != nil {
		t.Fatal(err)
	}
	d = s.OldestPendingAge(now)
	if d < 4*time.Minute || d > 6*time.Minute {
		t.Fatalf("after deciding oldest, expected ~5min, got %v", d)
	}

	// (2) 全部 decide 掉 → 0
	_, _ = s.Decide("middle", ActionApprove, "ops", "ok")
	_, _ = s.Decide("newest", ActionReject, "ops", "fraud")
	if d := s.OldestPendingAge(now); d != 0 {
		t.Fatalf("all decided should return 0, got %v", d)
	}
}

// TestMemStore_OldestPendingAge_NegativeClock 时钟漂移防御：
// CreatedAt 在 now 之后（系统时钟回拨）时返回 0，不返负数。
func TestMemStore_OldestPendingAge_NegativeClock(t *testing.T) {
	now := time.Now().UTC()
	s := NewMemStore()

	it := mkItem("future", "")
	it.CreatedAt = now.Add(1 * time.Hour)
	if err := s.Push(it); err != nil {
		t.Fatal(err)
	}

	if d := s.OldestPendingAge(now); d != 0 {
		t.Fatalf("CreatedAt > now should clamp to 0, got %v", d)
	}
}

// 静态接口断言：MemStore 必须实现完整的 Store。改 interface 时编译期失败。
var _ Store = (*MemStore)(nil)

// ─── ReasonCode validation ────────────────────────────────────────────────

func TestReasonCode_ValidAndList(t *testing.T) {
	// 列表至少 12 个（fraud_confirmed ... other）
	codes := ReasonCodes()
	if len(codes) < 12 {
		t.Fatalf("expected ≥12 reason codes, got %d", len(codes))
	}
	// 双语 label 都非空（i18n 完整性）
	for _, c := range codes {
		if c.ZhCN == "" || c.EnUS == "" {
			t.Fatalf("reason code %s missing label: zh=%q en=%q", c.Code, c.ZhCN, c.EnUS)
		}
	}
	// 校验函数
	if !ValidReasonCode(string(ReasonFraudConfirmed)) {
		t.Fatal("fraud_confirmed should be valid")
	}
	if ValidReasonCode("not_a_code") {
		t.Fatal("not_a_code should be invalid")
	}
	if ValidReasonCode("") {
		t.Fatal("empty code should be invalid")
	}
}

// ─── DecideWithCode ───────────────────────────────────────────────────────

func TestMemStore_DecideWithCode_Happy(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	it, err := s.DecideWithCode("a", ActionApprove, "alice", string(ReasonFalsePositive), "误杀，应放行")
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if it.Status != StatusApproved {
		t.Fatalf("expected approved, got %v", it.Status)
	}
	if it.ReasonCode != string(ReasonFalsePositive) {
		t.Fatalf("expected reason_code=false_positive, got %q", it.ReasonCode)
	}
	if it.DecideReason != "误杀，应放行" {
		t.Fatalf("free text not preserved: %q", it.DecideReason)
	}
}

func TestMemStore_DecideWithCode_InvalidReason(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	cases := []string{"", "bogus_code", "FRAUD_CONFIRMED" /* 大小写敏感 */}
	for _, code := range cases {
		_, err := s.DecideWithCode("a", ActionApprove, "alice", code, "")
		if !errors.Is(err, ErrInvalidReason) {
			t.Fatalf("code=%q: expected ErrInvalidReason, got %v", code, err)
		}
	}
	// 没动 state（still pending）
	if got := s.Get("a"); got.Status != StatusPending {
		t.Fatalf("state mutated despite reason error: %v", got.Status)
	}
}

// ─── Transfer ─────────────────────────────────────────────────────────────

func TestMemStore_Transfer_Happy(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	if _, err := s.Claim("a", "alice"); err != nil {
		t.Fatal(err)
	}
	it, err := s.Transfer("a", "alice", "bob", "alice 请假")
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if it.AssignedTo != "bob" {
		t.Fatalf("expected assigned_to=bob, got %q", it.AssignedTo)
	}
	if it.Status != StatusInReview {
		t.Fatalf("status should stay in_review, got %v", it.Status)
	}
	if len(it.TransferHist) != 1 {
		t.Fatalf("expected 1 transfer log, got %d", len(it.TransferHist))
	}
	if it.TransferHist[0].From != "alice" || it.TransferHist[0].To != "bob" {
		t.Fatalf("transfer log wrong: %+v", it.TransferHist[0])
	}
}

func TestMemStore_Transfer_NotAssignee(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	s.Claim("a", "alice")
	// bob 不是 assignee 尝试改派 → 失败
	if _, err := s.Transfer("a", "bob", "carol", "x"); !errors.Is(err, ErrNotAssignee) {
		t.Fatalf("expected ErrNotAssignee, got %v", err)
	}
}

func TestMemStore_Transfer_PendingRejected(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", "")) // 没人 claim
	if _, err := s.Transfer("a", "alice", "bob", "x"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("expected ErrNotPending on pending case, got %v", err)
	}
}

// ─── EscalateTo (L1 → L2) ─────────────────────────────────────────────────

func TestMemStore_EscalateTo_L1ToL2(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	s.Claim("a", "alice")
	it, err := s.EscalateTo("a", 2, "alice", "需要 senior review", EscalateTriggerManual)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if it.Level != 2 {
		t.Fatalf("expected level=2, got %d", it.Level)
	}
	if it.Status != StatusEscalated {
		t.Fatalf("expected escalated, got %v", it.Status)
	}
	if it.AssignedTo != "" {
		t.Fatalf("assigned_to should clear, got %q", it.AssignedTo)
	}
	if len(it.EscalateHist) != 1 {
		t.Fatalf("expected 1 escalate log, got %d", len(it.EscalateHist))
	}
	h := it.EscalateHist[0]
	if h.FromLevel != 1 || h.ToLevel != 2 || h.Trigger != EscalateTriggerManual || h.Actor != "alice" {
		t.Fatalf("escalate log wrong: %+v", h)
	}
}

func TestMemStore_EscalateTo_InvalidLevel(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	// 只支持 L2；L3 报错
	if _, err := s.EscalateTo("a", 3, "alice", "x", EscalateTriggerManual); !errors.Is(err, ErrInvalidLevel) {
		t.Fatalf("expected ErrInvalidLevel, got %v", err)
	}
}

func TestMemStore_EscalateTo_AlreadyAtLevel(t *testing.T) {
	s := NewMemStore()
	s.Push(mkItem("a", ""))
	s.EscalateTo("a", 2, "alice", "first", EscalateTriggerManual)
	// 再升一次 → 已经 L2 → ErrAlreadyAtLevel
	if _, err := s.EscalateTo("a", 2, "alice", "again", EscalateTriggerManual); !errors.Is(err, ErrAlreadyAtLevel) {
		t.Fatalf("expected ErrAlreadyAtLevel, got %v", err)
	}
}

// ─── AutoEscalateOverdue (SLA timeout) ────────────────────────────────────

func TestMemStore_AutoEscalateOverdue(t *testing.T) {
	s := NewMemStore()
	s.SetDefaultSLA(20 * time.Millisecond)
	// 三条 case：a/b 会过期，c push 完直接 decide 不参与
	s.Push(mkItem("a", ""))
	s.Push(mkItem("b", ""))
	s.Push(mkItem("c", ""))
	s.DecideWithCode("c", ActionApprove, "alice", string(ReasonFalsePositive), "")
	// 等过期
	time.Sleep(30 * time.Millisecond)

	n, err := s.AutoEscalateOverdue(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 auto-escalated, got %d", n)
	}
	a := s.Get("a")
	if a.Level != 2 || a.Status != StatusEscalated || !a.SLAEscalated {
		t.Fatalf("case a not properly auto-escalated: %+v", a)
	}
	if len(a.EscalateHist) != 1 || a.EscalateHist[0].Trigger != EscalateTriggerSLATimeout {
		t.Fatalf("expected sla_timeout trigger, got %+v", a.EscalateHist)
	}
	// c 已 decided，不受影响
	c := s.Get("c")
	if c.Level == 2 || c.SLAEscalated {
		t.Fatalf("decided case c shouldn't auto-escalate: %+v", c)
	}

	// 再跑一次：所有 overdue 都 SLAEscalated=true → 跳过
	n2, _ := s.AutoEscalateOverdue(context.Background(), time.Now().UTC())
	if n2 != 0 {
		t.Fatalf("second pass should escalate 0 (already escalated), got %d", n2)
	}
}

func TestMemStore_AutoEscalateOverdue_RespectsCtx(t *testing.T) {
	s := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消
	// 即使 ctx 取消，没有候选 case 时返 0 不报错（候选扫描在 ctx 检查前）
	n, err := s.AutoEscalateOverdue(ctx, time.Now())
	if n != 0 {
		t.Fatalf("expected 0 with cancelled ctx + empty store, got %d", n)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected err: %v", err)
	}
}

// ─── 集成：sla_escalator runOnce ───────────────────────────────────────────

func TestSLAEscalator_RunOnce_EscalatesOverdueCase(t *testing.T) {
	s := NewMemStore()
	s.SetDefaultSLA(10 * time.Millisecond)
	s.Push(mkItem("a", ""))
	time.Sleep(15 * time.Millisecond)

	// runOnce 是包内函数；直接调，不需要起 goroutine。
	ensureMetricsRegistered()
	runOnce(context.Background(), s, time.Now().UTC(), zap.NewNop())

	got := s.Get("a")
	if got.Level != 2 || !got.SLAEscalated {
		t.Fatalf("expected runOnce to escalate a to L2, got level=%d sla_escalated=%v",
			got.Level, got.SLAEscalated)
	}
}

