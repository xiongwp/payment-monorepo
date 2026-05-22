package model

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// LifecyclePhase 转换合法性表测试 — 覆盖整个转换图，确保 service 层之上的
// 守卫只能在合法边上附加业务规则，绝不放行非法边。
//
// 设计文档：§4 状态机 / §10.6 E-33 防误操作。
// ============================================================================

func TestCanTransitionPhase_AllEdgesExhaustive(t *testing.T) {
	allPhases := []LifecyclePhase{
		LifecyclePhaseLegacy,
		LifecyclePhaseProvisioned,
		LifecyclePhaseActive,
		LifecyclePhaseDraining,
		LifecyclePhaseFrozen,
		LifecyclePhaseArchived,
		LifecyclePhaseQuarantined,
	}

	// 期望合法的边（must match AllowedPhaseTransitions）
	expectedAllowed := map[[2]LifecyclePhase]bool{
		{LifecyclePhaseProvisioned, LifecyclePhaseActive}:       true,
		{LifecyclePhaseProvisioned, LifecyclePhaseQuarantined}:  true,
		{LifecyclePhaseActive, LifecyclePhaseDraining}:          true,
		{LifecyclePhaseActive, LifecyclePhaseQuarantined}:       true,
		{LifecyclePhaseDraining, LifecyclePhaseFrozen}:          true,
		{LifecyclePhaseDraining, LifecyclePhaseQuarantined}:     true,
		{LifecyclePhaseFrozen, LifecyclePhaseArchived}:          true,
		{LifecyclePhaseFrozen, LifecyclePhaseQuarantined}:       true,
		{LifecyclePhaseArchived, LifecyclePhaseQuarantined}:     true,
		{LifecyclePhaseQuarantined, LifecyclePhaseDraining}:     true,
		{LifecyclePhaseQuarantined, LifecyclePhaseFrozen}:       true,
		{LifecyclePhaseLegacy, LifecyclePhaseQuarantined}:       true,
	}

	for _, from := range allPhases {
		for _, to := range allPhases {
			want := expectedAllowed[[2]LifecyclePhase{from, to}]
			got := CanTransitionPhase(from, to)
			if got != want {
				t.Errorf("CanTransitionPhase(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestCanTransitionPhase_NoSelfLoops(t *testing.T) {
	allPhases := []LifecyclePhase{
		LifecyclePhaseLegacy, LifecyclePhaseProvisioned, LifecyclePhaseActive,
		LifecyclePhaseDraining, LifecyclePhaseFrozen, LifecyclePhaseArchived,
		LifecyclePhaseQuarantined,
	}
	for _, p := range allPhases {
		if CanTransitionPhase(p, p) {
			t.Errorf("self-transition allowed for %s; must be rejected", p)
		}
	}
}

// 关键不变量：禁止 active 跳过 draining 直达 frozen（E-33 误操作防线）。
func TestPhaseTransition_ForbidActiveToFrozen(t *testing.T) {
	if CanTransitionPhase(LifecyclePhaseActive, LifecyclePhaseFrozen) {
		t.Fatal("active->frozen must be illegal; defeats draining purpose & breaks I4 guards")
	}
}

// archived 是几乎终态，仅允许人工 quarantined（极端不一致拉回）。
func TestPhaseTransition_ArchivedIsTerminal(t *testing.T) {
	forbidden := []LifecyclePhase{
		LifecyclePhaseLegacy, LifecyclePhaseProvisioned,
		LifecyclePhaseActive, LifecyclePhaseDraining, LifecyclePhaseFrozen,
	}
	for _, to := range forbidden {
		if CanTransitionPhase(LifecyclePhaseArchived, to) {
			t.Errorf("archived->%s must be illegal", to)
		}
	}
	if !CanTransitionPhase(LifecyclePhaseArchived, LifecyclePhaseQuarantined) {
		t.Error("archived->quarantined must be allowed as last-resort manual escape")
	}
}

// draining 不允许回到 active（"复活"）；必须经过 quarantined 路径。
func TestPhaseTransition_NoDrainingToActive(t *testing.T) {
	if CanTransitionPhase(LifecyclePhaseDraining, LifecyclePhaseActive) {
		t.Fatal("draining->active must be illegal; revival path must go via quarantined")
	}
}

func TestValidatePhaseTransition_ErrorWrap(t *testing.T) {
	err := ValidatePhaseTransition(LifecyclePhaseActive, LifecyclePhaseArchived)
	if !errors.Is(err, ErrIllegalPhaseTransition) {
		t.Fatalf("expected wrapped ErrIllegalPhaseTransition, got %v", err)
	}
	if !strings.Contains(err.Error(), "active") || !strings.Contains(err.Error(), "archived") {
		t.Errorf("error should mention both phases for ops debugging, got %q", err.Error())
	}
}

// ============================================================================
// 写守卫语义测试
// ============================================================================

func TestLifecyclePhase_AcceptsNewAnchoring(t *testing.T) {
	cases := map[LifecyclePhase]bool{
		LifecyclePhaseActive:       true,
		LifecyclePhaseLegacy:       false, // legacy 走旧路径，不参与 anchor
		LifecyclePhaseProvisioned:  false,
		LifecyclePhaseDraining:     false,
		LifecyclePhaseFrozen:       false,
		LifecyclePhaseArchived:     false,
		LifecyclePhaseQuarantined:  false,
	}
	for p, want := range cases {
		if got := p.AcceptsNewAnchoring(); got != want {
			t.Errorf("phase=%s AcceptsNewAnchoring = %v, want %v", p, got, want)
		}
	}
}

func TestLifecyclePhase_AcceptsFollowupPosting(t *testing.T) {
	cases := map[LifecyclePhase]bool{
		LifecyclePhaseActive:      true,
		LifecyclePhaseDraining:    true, // 关键：3 天跨期支付走这条路径
		LifecyclePhaseLegacy:      false,
		LifecyclePhaseProvisioned: false,
		LifecyclePhaseFrozen:      false, // frozen TCC Cancel 例外由 router 单独走，不在此函数
		LifecyclePhaseArchived:    false,
		LifecyclePhaseQuarantined: false,
	}
	for p, want := range cases {
		if got := p.AcceptsFollowupPosting(); got != want {
			t.Errorf("phase=%s AcceptsFollowupPosting = %v, want %v", p, got, want)
		}
	}
}

func TestLifecyclePhase_String_NoUnknown(t *testing.T) {
	// 任何已声明的 phase 都不应该输出 "unknown(...)"
	declared := []LifecyclePhase{
		LifecyclePhaseLegacy, LifecyclePhaseProvisioned, LifecyclePhaseActive,
		LifecyclePhaseDraining, LifecyclePhaseFrozen, LifecyclePhaseArchived,
		LifecyclePhaseQuarantined,
	}
	for _, p := range declared {
		if strings.HasPrefix(p.String(), "unknown") {
			t.Errorf("declared phase %d has no String() entry", int8(p))
		}
	}
	// 但未声明的值应该回退到 unknown(N)
	rogue := LifecyclePhase(127)
	if !strings.HasPrefix(rogue.String(), "unknown") {
		t.Errorf("rogue value should fall back to unknown(N), got %q", rogue.String())
	}
}

// ============================================================================
// AnchorStatus 转换合法性表测试 — §3.4.1
// ============================================================================

func TestCanTransitionAnchor_AllEdgesExhaustive(t *testing.T) {
	all := []AnchorStatus{
		AnchorStatusTrying, AnchorStatusActive,
		AnchorStatusSettled, AnchorStatusMigrated, AnchorStatusStuck,
	}
	expected := map[[2]AnchorStatus]bool{
		{AnchorStatusTrying, AnchorStatusActive}:    true,
		{AnchorStatusTrying, AnchorStatusSettled}:   true, // TCC Cancel 完成
		{AnchorStatusTrying, AnchorStatusStuck}:     true,
		{AnchorStatusActive, AnchorStatusSettled}:   true,
		{AnchorStatusActive, AnchorStatusMigrated}:  true,
		{AnchorStatusActive, AnchorStatusStuck}:     true,
	}
	for _, f := range all {
		for _, t2 := range all {
			want := expected[[2]AnchorStatus{f, t2}]
			if got := CanTransitionAnchor(f, t2); got != want {
				t.Errorf("CanTransitionAnchor(%s, %s) = %v, want %v", f, t2, got, want)
			}
		}
	}
}

func TestAnchorStatus_TerminalStatesAreSticky(t *testing.T) {
	// settled / migrated 都是终态：不能再转出
	terminals := []AnchorStatus{AnchorStatusSettled, AnchorStatusMigrated}
	all := []AnchorStatus{
		AnchorStatusTrying, AnchorStatusActive,
		AnchorStatusSettled, AnchorStatusMigrated, AnchorStatusStuck,
	}
	for _, term := range terminals {
		for _, to := range all {
			if CanTransitionAnchor(term, to) {
				t.Errorf("terminal anchor status %s must not transition to %s", term, to)
			}
		}
	}
}

func TestAnchorStatus_StuckMustGoThroughOps(t *testing.T) {
	// stuck 不允许自动转出；只能由运维通过 quarantined 路径处理。
	// 这里验证 AllowedAnchorTransitions 没有 stuck 起点。
	if _, ok := AllowedAnchorTransitions[AnchorStatusStuck]; ok {
		t.Fatal("stuck anchor must require ops intervention; no automated transitions allowed")
	}
}

func TestAnchorStatus_IsOpen_StuckNotOpen(t *testing.T) {
	// 关键：stuck 不算 open（防止收敛 job 误计入）；
	// 但 instance 在 stuck > 0 时禁止 frozen 是另一道守卫（§7.1）。
	cases := map[AnchorStatus]bool{
		AnchorStatusTrying:   true,
		AnchorStatusActive:   true,
		AnchorStatusSettled:  false,
		AnchorStatusMigrated: false,
		AnchorStatusStuck:    false, // 关键
	}
	for s, want := range cases {
		if got := s.IsOpen(); got != want {
			t.Errorf("anchor status=%s IsOpen = %v, want %v", s, got, want)
		}
	}
}

func TestValidateAnchorTransition_ErrorWrap(t *testing.T) {
	err := ValidateAnchorTransition(AnchorStatusSettled, AnchorStatusActive)
	if !errors.Is(err, ErrIllegalAnchorTransition) {
		t.Fatalf("expected wrapped ErrIllegalAnchorTransition, got %v", err)
	}
}

// ============================================================================
// AnchorDirectionMask 位运算
// ============================================================================

func TestAnchorDirectionMask_Composition(t *testing.T) {
	var m AnchorDirectionMask
	if m.HasDebit() || m.HasCredit() {
		t.Fatal("zero mask must report no flags")
	}

	m = m.WithDebit()
	if !m.HasDebit() || m.HasCredit() {
		t.Fatal("after WithDebit, only debit should be set")
	}

	m = m.WithCredit()
	if !m.HasDebit() || !m.HasCredit() {
		t.Fatal("after WithCredit, both flags should be set")
	}

	// idempotent: re-applying must not flip bits
	m2 := m.WithDebit().WithCredit()
	if m != m2 {
		t.Errorf("WithDebit/WithCredit must be idempotent; got %d vs %d", m, m2)
	}

	// values match documented bit layout
	if AnchorDirectionDebit != 1 || AnchorDirectionCredit != 2 {
		t.Errorf("bit layout drifted: debit=%d credit=%d", AnchorDirectionDebit, AnchorDirectionCredit)
	}
}

// ============================================================================
// LogicalAccount key 校验 — §3.1 / E-47
// ============================================================================

func TestValidateLogicalAccountKey_AcceptsAllowedPrefixes(t *testing.T) {
	good := []string{
		"transit:default:USD",
		"channel-payable:alipay:CNY",
		"channel-receivable:stripe:USD",
	}
	for _, k := range good {
		if err := ValidateLogicalAccountKey(k); err != nil {
			t.Errorf("expected %q to be valid, got %v", k, err)
		}
	}
}

func TestValidateLogicalAccountKey_RejectsTypoAndMissingPrefix(t *testing.T) {
	bad := []string{
		"",                                   // empty
		"transit",                            // too short, no colon
		"unknown-prefix:something:USD",       // wrong prefix
		"transit",                            // 7 chars, below min
		"channel-payable:" + strings.Repeat("x", 60), // > 64
		"transit:has space",                  // whitespace
		"channel-payable:中文",                  // non-ASCII
	}
	for _, k := range bad {
		if err := ValidateLogicalAccountKey(k); err == nil {
			t.Errorf("expected %q to be rejected, but got nil error", k)
		}
	}
}

func TestValidateLogicalAccountKey_LengthBoundaries(t *testing.T) {
	// exact min length (8): "transit:" = 8 chars but ends at boundary; need 8 chars TOTAL
	exactlyMin := "transit:" // len = 8
	if err := ValidateLogicalAccountKey(exactlyMin); err != nil {
		t.Errorf("len-8 key with prefix must be valid: %v", err)
	}
	// 7 chars: too short
	tooShort := "transit"
	if err := ValidateLogicalAccountKey(tooShort); err == nil {
		t.Error("len-7 key must be rejected")
	}
	// exact max length (64)
	suffix := strings.Repeat("a", 64-len("transit:"))
	exactlyMax := "transit:" + suffix
	if len(exactlyMax) != 64 {
		t.Fatalf("test setup error: built len=%d, expected 64", len(exactlyMax))
	}
	if err := ValidateLogicalAccountKey(exactlyMax); err != nil {
		t.Errorf("len-64 key must be valid: %v", err)
	}
	// 65 chars: too long
	tooLong := exactlyMax + "x"
	if err := ValidateLogicalAccountKey(tooLong); err == nil {
		t.Error("len-65 key must be rejected")
	}
}

// ============================================================================
// LogicalAccountRotationPolicy.Validate
// ============================================================================

func TestPolicyValidate_HappyPath(t *testing.T) {
	p := &LogicalAccountRotationPolicy{
		PeriodUnit:           PeriodUnitMonth,
		PeriodCount:          1,
		RotationAnchorTZ:     "Asia/Shanghai",
		DrainP99Seconds:      7 * 86400,
		DrainHardTimeoutSecs: 30 * 86400,
		ArchiveGraceSecs:     7 * 86400,
		ProvisionLeadSecs:    86400,
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("happy-path policy should validate, got %v", err)
	}
}

func TestPolicyValidate_RejectsInvalid(t *testing.T) {
	base := LogicalAccountRotationPolicy{
		PeriodUnit:           PeriodUnitMonth,
		PeriodCount:          1,
		RotationAnchorTZ:     "Asia/Shanghai",
		DrainP99Seconds:      100,
		DrainHardTimeoutSecs: 200,
		ArchiveGraceSecs:     0,
		ProvisionLeadSecs:    0,
	}

	mutations := []struct {
		name   string
		mutate func(p *LogicalAccountRotationPolicy)
	}{
		{"bad period_unit", func(p *LogicalAccountRotationPolicy) { p.PeriodUnit = "FORTNIGHT" }},
		{"zero period_count", func(p *LogicalAccountRotationPolicy) { p.PeriodCount = 0 }},
		{"negative period_count", func(p *LogicalAccountRotationPolicy) { p.PeriodCount = -1 }},
		{"empty tz", func(p *LogicalAccountRotationPolicy) { p.RotationAnchorTZ = "" }},
		{"bogus tz", func(p *LogicalAccountRotationPolicy) { p.RotationAnchorTZ = "Mars/Olympus" }},
		{"zero p99", func(p *LogicalAccountRotationPolicy) { p.DrainP99Seconds = 0 }},
		{"hard < p99", func(p *LogicalAccountRotationPolicy) {
			p.DrainP99Seconds = 200
			p.DrainHardTimeoutSecs = 100
		}},
		{"negative grace", func(p *LogicalAccountRotationPolicy) { p.ArchiveGraceSecs = -1 }},
		{"negative provision lead", func(p *LogicalAccountRotationPolicy) { p.ProvisionLeadSecs = -1 }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			p := base
			m.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Errorf("mutation %q should produce error, got nil", m.name)
			}
		})
	}
}

func TestPeriodUnit_IsValid(t *testing.T) {
	for _, u := range []PeriodUnit{PeriodUnitDay, PeriodUnitMonth, PeriodUnitQuarter} {
		if !u.IsValid() {
			t.Errorf("documented PeriodUnit %q not valid", u)
		}
	}
	for _, bad := range []PeriodUnit{"", "WEEK", "YEAR", "month"} {
		if PeriodUnit(bad).IsValid() {
			t.Errorf("unknown PeriodUnit %q must be invalid", bad)
		}
	}
}

// ============================================================================
// TxAccountAnchor 行为
// ============================================================================

func TestTxAccountAnchor_EffectiveAccountNo(t *testing.T) {
	a := &TxAccountAnchor{
		AccountNo: "A001",
		Status:    AnchorStatusActive,
	}
	if got := a.EffectiveAccountNo(); got != "A001" {
		t.Errorf("active anchor effective acc must be original, got %q", got)
	}

	migrated := "A042"
	a2 := &TxAccountAnchor{
		AccountNo:           "A001",
		Status:              AnchorStatusMigrated,
		MigratedToAccountNo: &migrated,
	}
	if got := a2.EffectiveAccountNo(); got != migrated {
		t.Errorf("migrated anchor should report migrated_to, got %q", got)
	}

	// 防御：migrated 状态但 migrated_to 是 nil（应该被不变量检查捕获，
	// 但 EffectiveAccountNo 不应 panic，而是回退到原 account_no）。
	a3 := &TxAccountAnchor{AccountNo: "A001", Status: AnchorStatusMigrated}
	if got := a3.EffectiveAccountNo(); got != "A001" {
		t.Errorf("migrated with nil migrated_to should not panic; got %q", got)
	}
}

func TestTxAccountAnchor_OpenStuckMigrated(t *testing.T) {
	cases := []struct {
		status                          AnchorStatus
		wantOpen, wantStuck, wantMigrated bool
	}{
		{AnchorStatusTrying, true, false, false},
		{AnchorStatusActive, true, false, false},
		{AnchorStatusSettled, false, false, false},
		{AnchorStatusMigrated, false, false, true},
		{AnchorStatusStuck, false, true, false},
	}
	for _, c := range cases {
		a := &TxAccountAnchor{Status: c.status}
		if got := a.IsOpen(); got != c.wantOpen {
			t.Errorf("status=%s IsOpen=%v want %v", c.status, got, c.wantOpen)
		}
		if got := a.IsStuck(); got != c.wantStuck {
			t.Errorf("status=%s IsStuck=%v want %v", c.status, got, c.wantStuck)
		}
		if got := a.IsMigrated(); got != c.wantMigrated {
			t.Errorf("status=%s IsMigrated=%v want %v", c.status, got, c.wantMigrated)
		}
	}
}

// ============================================================================
// LogicalAccount helpers
// ============================================================================

func TestLogicalAccount_IsRotating(t *testing.T) {
	rotating := &LogicalAccount{RotationEnabled: 1}
	legacy := &LogicalAccount{RotationEnabled: 0}
	if !rotating.IsRotating() {
		t.Error("rotation_enabled=1 must report IsRotating")
	}
	if legacy.IsRotating() {
		t.Error("rotation_enabled=0 must NOT report IsRotating")
	}
}

func TestLogicalAccount_IsEnabled(t *testing.T) {
	if (&LogicalAccount{Status: LogicalAccountStatusDisabled}).IsEnabled() {
		t.Error("disabled status must not report IsEnabled")
	}
	if !(&LogicalAccount{Status: LogicalAccountStatusEnabled}).IsEnabled() {
		t.Error("enabled status must report IsEnabled")
	}
}

func TestLogicalAccount_TableName(t *testing.T) {
	if (LogicalAccount{}).TableName() != "logical_account" {
		t.Errorf("table name drift: got %q", (LogicalAccount{}).TableName())
	}
	if (TxAccountAnchor{}).TableName() != "tx_account_anchor" {
		t.Errorf("table name drift: got %q", (TxAccountAnchor{}).TableName())
	}
	if (LogicalAccountRotationPolicy{}).TableName() != "logical_account_rotation_policy" {
		t.Errorf("table name drift: got %q", (LogicalAccountRotationPolicy{}).TableName())
	}
}

// TestBusinessTypeConstants_Stable 删除：原来覆盖的 AccountBusinessType 10-13
// (MigrationSuspense / ResidualWriteOff / RotationOpsAdjust / RotationCarryforward)
// 已经从 model 删除，不再占 business_type registry 名额。

// ============================================================================
// 不变量校验工具（被 property-based fuzz 测复用 — Batch 10）
// ============================================================================

// AnchorTransitionPath 模拟一个 anchor 从初始状态到最终状态的转换序列。
// 验证：任意序列要么全部合法，要么在第一个非法步骤断言失败。
func walkAnchorTransitions(t *testing.T, start AnchorStatus, steps []AnchorStatus, wantOK bool) {
	t.Helper()
	cur := start
	for i, next := range steps {
		ok := CanTransitionAnchor(cur, next)
		if !ok {
			if wantOK {
				t.Fatalf("step %d: %s -> %s rejected unexpectedly", i, cur, next)
			}
			return
		}
		cur = next
	}
	if !wantOK {
		t.Fatalf("full path %v unexpectedly succeeded ending at %s", steps, cur)
	}
}

func TestAnchorTransitions_GoldenPaths(t *testing.T) {
	// 黄金路径 1: TCC Try -> Confirm -> Settle
	walkAnchorTransitions(t, AnchorStatusTrying,
		[]AnchorStatus{AnchorStatusActive, AnchorStatusSettled}, true)

	// 黄金路径 2: TCC Try -> Cancel (直接 settle)
	walkAnchorTransitions(t, AnchorStatusTrying,
		[]AnchorStatus{AnchorStatusSettled}, true)

	// 黄金路径 3: 长尾迁移 — Active -> Migrated
	walkAnchorTransitions(t, AnchorStatusTrying,
		[]AnchorStatus{AnchorStatusActive, AnchorStatusMigrated}, true)

	// 异常路径: settled 不可逆
	walkAnchorTransitions(t, AnchorStatusTrying,
		[]AnchorStatus{AnchorStatusActive, AnchorStatusSettled, AnchorStatusActive}, false)

	// 异常路径: stuck 不允许自动转出
	walkAnchorTransitions(t, AnchorStatusTrying,
		[]AnchorStatus{AnchorStatusStuck, AnchorStatusActive}, false)
}

func TestPhaseTransitions_GoldenPath(t *testing.T) {
	// Provisioned -> Active -> Draining -> Frozen -> Archived
	path := []LifecyclePhase{
		LifecyclePhaseProvisioned,
		LifecyclePhaseActive,
		LifecyclePhaseDraining,
		LifecyclePhaseFrozen,
		LifecyclePhaseArchived,
	}
	for i := 0; i+1 < len(path); i++ {
		if !CanTransitionPhase(path[i], path[i+1]) {
			t.Fatalf("golden path step %d: %s -> %s should be allowed",
				i, path[i], path[i+1])
		}
	}
}

// Ensure key constant time.Time zero-handling doesn't break Validate paths
func TestPolicyValidate_TimezoneEdgeCases(t *testing.T) {
	for _, tz := range []string{"UTC", "Asia/Shanghai", "Europe/London", "America/Los_Angeles"} {
		p := &LogicalAccountRotationPolicy{
			PeriodUnit:           PeriodUnitMonth,
			PeriodCount:          1,
			RotationAnchorTZ:     tz,
			DrainP99Seconds:      1,
			DrainHardTimeoutSecs: 1,
		}
		if err := p.Validate(); err != nil {
			t.Errorf("tz %q must be valid IANA: %v", tz, err)
		}
		// sanity: LoadLocation matches
		if _, err := time.LoadLocation(tz); err != nil {
			t.Errorf("test setup: tz %q failed to LoadLocation: %v", tz, err)
		}
	}
}

// ============================================================================
// EDGE CASES — 补充覆盖
// ============================================================================

// LogicalAccount RotationEnabled 仅 0/1 合法；其他值不应被认为是"rotating"。
// 防御 DB 数据污染或反序列化错误。
func TestLogicalAccount_IsRotating_NonBinaryValues(t *testing.T) {
	cases := []struct {
		v    int8
		want bool
	}{
		{0, false},
		{1, true},
		{2, false}, // 不能把任意非零都当 true，否则未来加新值会破坏语义
		{-1, false},
		{127, false},
	}
	for _, c := range cases {
		la := &LogicalAccount{RotationEnabled: c.v}
		if got := la.IsRotating(); got != c.want {
			t.Errorf("RotationEnabled=%d IsRotating=%v want %v", c.v, got, c.want)
		}
	}
}

// Account IsLegacy/IsRotating 在脏数据组合下也要给出确定性答案。
// 不允许 panic，不允许两个方法同时返回 true。
func TestAccount_LegacyRotatingCombinations(t *testing.T) {
	la := int64(42)
	cases := []struct {
		name        string
		laID        *int64
		phase       LifecyclePhase
		wantLegacy  bool
		wantRotate  bool
	}{
		{"nil + legacy (旧账户经典)", nil, LifecyclePhaseLegacy, true, false},
		{"nil + active (数据污染)", nil, LifecyclePhaseActive, true, false},
		{"set + legacy (数据污染)", &la, LifecyclePhaseLegacy, true, false},
		{"set + active (轮换正常)", &la, LifecyclePhaseActive, false, true},
		{"set + draining", &la, LifecyclePhaseDraining, false, true},
		{"set + frozen", &la, LifecyclePhaseFrozen, false, true},
		{"set + archived", &la, LifecyclePhaseArchived, false, true},
		{"set + quarantined", &la, LifecyclePhaseQuarantined, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Account{LogicalAccountID: c.laID, LifecyclePhase: c.phase}
			gotLegacy := a.IsLegacy()
			gotRotate := a.IsRotating()
			if gotLegacy != c.wantLegacy {
				t.Errorf("IsLegacy=%v want %v", gotLegacy, c.wantLegacy)
			}
			if gotRotate != c.wantRotate {
				t.Errorf("IsRotating=%v want %v", gotRotate, c.wantRotate)
			}
			// 互斥
			if gotLegacy && gotRotate {
				t.Errorf("must not be both legacy AND rotating")
			}
		})
	}
}

// Account.AcceptsNewAnchoring / AcceptsFollowupPosting 在 legacy 情况下永远 false。
func TestAccount_AcceptsLegacyAlwaysFalse(t *testing.T) {
	a := &Account{LifecyclePhase: LifecyclePhaseLegacy} // 无 logical_account_id
	if a.AcceptsNewAnchoring() {
		t.Error("legacy must NOT accept new anchoring")
	}
	if a.AcceptsFollowupPosting() {
		t.Error("legacy must NOT accept followup posting via rotation path")
	}
}

// ValidateLogicalAccountKey 控制字符与 ASCII 边界。
//  - 0x20 (space) → 拒（属于 whitespace 范畴）
//  - 0x21 (!) → 允（最低可打印字符，但仍需前缀匹配）
//  - 0x7E (~) → 允
//  - 0x7F (DEL) → 拒
//  - 0x00 (NUL) → 拒
func TestValidateLogicalAccountKey_AsciiBoundaries(t *testing.T) {
	withChar := func(c byte) string {
		return "transit:" + string(c)
	}
	cases := []struct {
		c     byte
		valid bool
	}{
		{0x00, false}, // NUL
		{0x09, false}, // TAB
		{0x0A, false}, // LF
		{0x1F, false}, // unit separator
		{0x20, false}, // space
		{0x21, true},  // '!'
		{0x7E, true},  // '~'
		{0x7F, false}, // DEL
		{0x80, false}, // 高位 — non-ASCII
		{0xFF, false},
	}
	for _, c := range cases {
		key := withChar(c.c)
		err := ValidateLogicalAccountKey(key)
		if c.valid && err != nil {
			t.Errorf("char 0x%02X should be valid in key, got err: %v", c.c, err)
		}
		if !c.valid && err == nil {
			t.Errorf("char 0x%02X should be REJECTED in key, got nil err", c.c)
		}
	}
}

// AnchorDirectionMask 位运算在脏数据上也要表现稳定。
func TestAnchorDirectionMask_DirtyValues(t *testing.T) {
	// mask=3 = both bits set
	both := AnchorDirectionMask(3)
	if !both.HasDebit() || !both.HasCredit() {
		t.Errorf("mask=3 should report both bits, got debit=%v credit=%v",
			both.HasDebit(), both.HasCredit())
	}
	// 高位脏数据，但仍能正确判断低位
	dirty := AnchorDirectionMask(127)
	if !dirty.HasDebit() || !dirty.HasCredit() {
		t.Errorf("mask=127 has all low bits, should report both")
	}
	// mask 仅高位污染，低位为 0
	highOnly := AnchorDirectionMask(0x7C) // 0b01111100，低 2 位 = 0
	if highOnly.HasDebit() || highOnly.HasCredit() {
		t.Errorf("mask=0x7C with low bits=0 should report no flags; got debit=%v credit=%v",
			highOnly.HasDebit(), highOnly.HasCredit())
	}
}

// LifecyclePhase.String 在 int8 极限值下不能 panic。
func TestLifecyclePhase_String_ExtremeValues(t *testing.T) {
	values := []LifecyclePhase{-128, -1, 0, 1, 9, 100, 127}
	for _, v := range values {
		// 不能 panic
		got := v.String()
		if got == "" {
			t.Errorf("phase=%d String() empty", v)
		}
	}
}

// AnchorStatus.String 同上。
func TestAnchorStatus_String_ExtremeValues(t *testing.T) {
	values := []AnchorStatus{-128, -1, 0, 1, 4, 99, 127}
	for _, v := range values {
		got := v.String()
		if got == "" {
			t.Errorf("status=%d String() empty", v)
		}
	}
}

// AnchorReuseSource.String 同上。
func TestAnchorReuseSource_String_ExtremeValues(t *testing.T) {
	for _, v := range []AnchorReuseSource{-1, 0, 1, 2, 3, 127} {
		got := v.String()
		if got == "" {
			t.Errorf("reuse_source=%d String() empty", v)
		}
	}
}

// EffectiveAccountNo 在 migrated 但 migrated_to_account_no=nil 时不能 panic，
// 必须回退到 AccountNo。AccountNo 为空时返回空字符串（防御性）。
func TestTxAccountAnchor_EffectiveAccountNo_EmptyOriginal(t *testing.T) {
	a := &TxAccountAnchor{AccountNo: "", Status: AnchorStatusActive}
	if got := a.EffectiveAccountNo(); got != "" {
		t.Errorf("empty AccountNo should yield empty; got %q", got)
	}
	// migrated 状态但 migrated_to=nil 且 AccountNo="" — 也是空
	a2 := &TxAccountAnchor{AccountNo: "", Status: AnchorStatusMigrated}
	if got := a2.EffectiveAccountNo(); got != "" {
		t.Errorf("migrated with nil migrated_to and empty AccountNo: got %q", got)
	}
}

// Policy.Validate: 极限数值
func TestPolicyValidate_ExtremeNumericValues(t *testing.T) {
	// MaxInt 边界（用 32 位上限避免平台差异）
	const maxInt32 = 2147483647
	p := &LogicalAccountRotationPolicy{
		PeriodUnit:           PeriodUnitMonth,
		PeriodCount:          maxInt32,
		RotationAnchorTZ:     "UTC",
		DrainP99Seconds:      maxInt32 - 1,
		DrainHardTimeoutSecs: maxInt32,
		ArchiveGraceSecs:     maxInt32,
		ProvisionLeadSecs:    maxInt32,
	}
	if err := p.Validate(); err != nil {
		t.Errorf("MaxInt32 boundary should pass validate, got %v", err)
	}

	// drain_hard == p99（等于也允许，严格 < 才拒）
	p2 := &LogicalAccountRotationPolicy{
		PeriodUnit:           PeriodUnitMonth,
		PeriodCount:          1,
		RotationAnchorTZ:     "UTC",
		DrainP99Seconds:      100,
		DrainHardTimeoutSecs: 100,
	}
	if err := p2.Validate(); err != nil {
		t.Errorf("drain_hard == p99 should be valid (equal), got %v", err)
	}

	// archive_grace = 0（即时归档，允许）
	p3 := &LogicalAccountRotationPolicy{
		PeriodUnit:           PeriodUnitMonth,
		PeriodCount:          1,
		RotationAnchorTZ:     "UTC",
		DrainP99Seconds:      1,
		DrainHardTimeoutSecs: 1,
		ArchiveGraceSecs:     0,
	}
	if err := p3.Validate(); err != nil {
		t.Errorf("archive_grace=0 should be valid (immediate archive), got %v", err)
	}
}

// PeriodUnit 大小写敏感
func TestPeriodUnit_CaseSensitive(t *testing.T) {
	if PeriodUnit("month").IsValid() {
		t.Error("lowercase 'month' must NOT be valid (case sensitive)")
	}
	if PeriodUnit("MONTH").IsValid() == false {
		t.Error("uppercase MONTH must be valid")
	}
}

// 全 5x5 anchor 转换矩阵穷举（除了 trying/active 起点已在 allowed 表中，其余都应拒）
func TestAnchorTransitions_ExhaustiveMatrix(t *testing.T) {
	all := []AnchorStatus{
		AnchorStatusTrying, AnchorStatusActive,
		AnchorStatusSettled, AnchorStatusMigrated, AnchorStatusStuck,
	}
	allowed := map[[2]AnchorStatus]bool{
		{AnchorStatusTrying, AnchorStatusActive}:    true,
		{AnchorStatusTrying, AnchorStatusSettled}:   true,
		{AnchorStatusTrying, AnchorStatusStuck}:     true,
		{AnchorStatusActive, AnchorStatusSettled}:   true,
		{AnchorStatusActive, AnchorStatusMigrated}:  true,
		{AnchorStatusActive, AnchorStatusStuck}:     true,
	}
	for _, f := range all {
		for _, t2 := range all {
			want := allowed[[2]AnchorStatus{f, t2}]
			got := CanTransitionAnchor(f, t2)
			if got != want {
				t.Errorf("CanTransitionAnchor(%s,%s) = %v, want %v", f, t2, got, want)
			}
		}
	}
}

// hasPrefix（rotation.go 里的本地辅助）边界
func TestHasPrefix_Boundaries(t *testing.T) {
	cases := []struct {
		s, prefix string
		want      bool
	}{
		{"", "", true},
		{"", "x", false},
		{"x", "", true},
		{"x", "x", true},
		{"abc", "abcd", false},
		{"abcd", "abc", true},
		{"a", "A", false}, // 大小写敏感
	}
	for _, c := range cases {
		if got := hasPrefix(c.s, c.prefix); got != c.want {
			t.Errorf("hasPrefix(%q, %q) = %v, want %v", c.s, c.prefix, got, c.want)
		}
	}
}

// ============================================================================
// PROPERTY-BASED TESTS — 随机序列必须保持不变量
//
// 用 testing/quick 风格的手摇随机：对任意（合法）转换序列，每一步必须满足
// CanTransitionPhase；任何序列结束在 archived 的下一步只能是 quarantined（终态）。
// ============================================================================

// 任意起始 phase 走任意一条合法路径，过程中不变量恒成立：
//   1. 每一步都在 AllowedPhaseTransitions 中
//   2. archived 后不能再走非 quarantined
//   3. 序列长度上限触发不会被绕过
func TestPropertyBased_PhaseSequenceInvariants(t *testing.T) {
	allPhases := []LifecyclePhase{
		LifecyclePhaseProvisioned, LifecyclePhaseActive,
		LifecyclePhaseDraining, LifecyclePhaseFrozen, LifecyclePhaseArchived,
		LifecyclePhaseQuarantined,
	}

	// 简单 LCG，确定性可复现的伪随机
	seed := uint64(0xCAFEBABE)
	nextRand := func() uint64 {
		seed = seed*6364136223846793005 + 1442695040888963407
		return seed
	}

	const iterations = 5000
	for it := 0; it < iterations; it++ {
		current := allPhases[nextRand()%uint64(len(allPhases))]
		path := []LifecyclePhase{current}
		steps := int(nextRand()%10) + 1
		for step := 0; step < steps; step++ {
			target := allPhases[nextRand()%uint64(len(allPhases))]
			if !CanTransitionPhase(current, target) {
				continue
			}
			path = append(path, target)
			current = target
		}
		// invariant 1: 每一步都合法
		for i := 0; i+1 < len(path); i++ {
			if !CanTransitionPhase(path[i], path[i+1]) {
				t.Fatalf("iter=%d path=%v illegal step %d: %s->%s",
					it, path, i, path[i], path[i+1])
			}
		}
		// invariant 2: archived 之后只能是 quarantined
		for i := 0; i+1 < len(path); i++ {
			if path[i] == LifecyclePhaseArchived && path[i+1] != LifecyclePhaseQuarantined {
				t.Fatalf("iter=%d path=%v: archived followed by non-quarantined %s",
					it, path, path[i+1])
			}
		}
	}
}

// 同样的 property 测试 anchor.status
func TestPropertyBased_AnchorSequenceInvariants(t *testing.T) {
	all := []AnchorStatus{
		AnchorStatusTrying, AnchorStatusActive,
		AnchorStatusSettled, AnchorStatusMigrated, AnchorStatusStuck,
	}

	seed := uint64(0xDEADBEEF)
	nextRand := func() uint64 {
		seed = seed*6364136223846793005 + 1442695040888963407
		return seed
	}

	const iterations = 5000
	for it := 0; it < iterations; it++ {
		current := all[nextRand()%uint64(len(all))]
		visited := []AnchorStatus{current}
		for step := 0; step < 10; step++ {
			target := all[nextRand()%uint64(len(all))]
			if !CanTransitionAnchor(current, target) {
				continue
			}
			visited = append(visited, target)
			current = target
		}
		// invariant: 终态（settled / migrated）后不能再有合法转换
		for i := 0; i+1 < len(visited); i++ {
			if visited[i].IsTerminal() {
				t.Fatalf("iter=%d path=%v: terminal %s has follower %s",
					it, visited, visited[i], visited[i+1])
			}
		}
	}
}

// 验证 CanTransitionPhase 对每个 (from, to) 对的结果与 AllowedPhaseTransitions
// map 严格一致（无遗漏、无虚假合法）。
func TestCanTransitionPhase_MapAuthorityCheck(t *testing.T) {
	all := []LifecyclePhase{
		LifecyclePhaseLegacy, LifecyclePhaseProvisioned, LifecyclePhaseActive,
		LifecyclePhaseDraining, LifecyclePhaseFrozen, LifecyclePhaseArchived,
		LifecyclePhaseQuarantined,
	}
	for _, from := range all {
		expected := AllowedPhaseTransitions[from] // nil if not present
		// 在不在 expected 列表里
		expectedSet := make(map[LifecyclePhase]bool)
		for _, p := range expected {
			expectedSet[p] = true
		}
		for _, to := range all {
			got := CanTransitionPhase(from, to)
			want := expectedSet[to] && from != to // CanTransitionPhase 显式禁自环
			if got != want {
				t.Errorf("CanTransitionPhase(%s,%s)=%v but map says %v (selfLoop=%v)",
					from, to, got, want, from == to)
			}
		}
	}
}

// AnchorStatus.IsOpen 与 AllowedAnchorTransitions 起点必须一致：
// IsOpen=true 的状态都必须在 map 里有合法转换；反之 IsOpen=false 不在 map。
func TestAnchorStatus_IsOpenMatchesTransitionMap(t *testing.T) {
	for _, s := range []AnchorStatus{
		AnchorStatusTrying, AnchorStatusActive,
		AnchorStatusSettled, AnchorStatusMigrated, AnchorStatusStuck,
	} {
		_, hasMapping := AllowedAnchorTransitions[s]
		if s.IsOpen() != hasMapping {
			t.Errorf("anchor %s: IsOpen=%v but transition map has=%v", s, s.IsOpen(), hasMapping)
		}
	}
}

// TestNewBusinessTypes_DocumentedAccountTypes 删除：覆盖的是已删除的 4 个常量
// (AccountBusinessType 10-13)。删除原因见 model rotation.go 同位置注释。

// ============================================================================
// LogicalAccount 严格性：禁止某些组合
// ============================================================================

// Currency 校验委托给 caller（Validate 不做），但 Description 是 *string 允 nil。
// 验证 LogicalAccount 实例化不会 panic（基本构造健壮性）。
func TestLogicalAccount_ConstructionWithNullableFields(t *testing.T) {
	la := &LogicalAccount{
		LogicalAccountKey:   "transit:test:USD",
		AccountType:         AccountTypeTransit,
		AccountBusinessType: AccountBusinessTypeTransit,
		Currency:            "USD",
		RotationEnabled:     1,
		RegisteredBy:        "ops",
		Status:              LogicalAccountStatusEnabled,
		// Description / CurrentActive* 留 nil
	}
	if !la.IsRotating() {
		t.Error("rotation_enabled=1 should report IsRotating")
	}
	if !la.IsEnabled() {
		t.Error("enabled status should report IsEnabled")
	}
}

// ============================================================================
// DirectionMask 全 256 字节穷举：只看低 2 位，高位不该影响 HasDebit/HasCredit
// ============================================================================
func TestDirectionMask_All256BytesLow2BitsOnly(t *testing.T) {
	for v := 0; v < 256; v++ {
		m := AnchorDirectionMask(int8(v))
		// 我们的 mask 字段是 int8（-128..127），强制位运算
		expectedDebit := v&1 != 0
		expectedCredit := v&2 != 0
		if m.HasDebit() != expectedDebit {
			t.Errorf("mask=0x%02x: HasDebit=%v want %v", v, m.HasDebit(), expectedDebit)
		}
		if m.HasCredit() != expectedCredit {
			t.Errorf("mask=0x%02x: HasCredit=%v want %v", v, m.HasCredit(), expectedCredit)
		}
	}
}

// WithDebit/WithCredit 幂等性：多次应用结果稳定
func TestDirectionMask_WithMethods_Idempotent(t *testing.T) {
	for v := 0; v < 256; v++ {
		m := AnchorDirectionMask(int8(v))
		a := m.WithDebit().WithDebit().WithDebit()
		b := m.WithDebit()
		if a != b {
			t.Errorf("mask=0x%02x: WithDebit not idempotent: triple=%d single=%d", v, a, b)
		}
		c := m.WithCredit().WithCredit()
		d := m.WithCredit()
		if c != d {
			t.Errorf("mask=0x%02x: WithCredit not idempotent: double=%d single=%d", v, c, d)
		}
	}
}

// ============================================================================
// LifecyclePhase 终态语义
// ============================================================================

func TestLifecyclePhase_IsTerminal_ArchivedOnly(t *testing.T) {
	terminals := map[LifecyclePhase]bool{
		LifecyclePhaseLegacy:      false,
		LifecyclePhaseProvisioned: false,
		LifecyclePhaseActive:      false,
		LifecyclePhaseDraining:    false,
		LifecyclePhaseFrozen:      false,
		LifecyclePhaseArchived:    true, // 唯一终态
		LifecyclePhaseQuarantined: false,
	}
	for p, want := range terminals {
		if p.IsTerminal() != want {
			t.Errorf("IsTerminal(%s)=%v want %v", p, p.IsTerminal(), want)
		}
	}
}

// AnchorStatus.IsTerminal: settled / migrated 是终态
func TestAnchorStatus_IsTerminal_Coverage(t *testing.T) {
	cases := map[AnchorStatus]bool{
		AnchorStatusTrying:   false,
		AnchorStatusActive:   false,
		AnchorStatusSettled:  true, // 终态
		AnchorStatusMigrated: true, // 终态
		AnchorStatusStuck:    false, // 等待人工介入，不算终态
	}
	for s, want := range cases {
		if s.IsTerminal() != want {
			t.Errorf("IsTerminal(%s)=%v want %v", s, s.IsTerminal(), want)
		}
	}
}
