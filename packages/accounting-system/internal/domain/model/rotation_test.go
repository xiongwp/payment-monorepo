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

// ============================================================================
// 新预置 AccountBusinessType 常量值稳定性（防止后续误改）
// ============================================================================

func TestBusinessTypeConstants_Stable(t *testing.T) {
	// 这些值落到 DB 后变更会破坏对账。一旦合入主干禁止改动。
	cases := []struct {
		name string
		v    AccountBusinessType
		want AccountBusinessType
	}{
		{"MigrationSuspense", AccountBusinessTypeMigrationSuspense, 10},
		{"ResidualWriteOff", AccountBusinessTypeResidualWriteOff, 11},
		{"RotationOpsAdjust", AccountBusinessTypeRotationOpsAdjust, 12},
		{"RotationCarryforward", AccountBusinessTypeRotationCarryforward, 13},
	}
	for _, c := range cases {
		if c.v != c.want {
			t.Errorf("%s must be %d (DB-stable); got %d", c.name, c.want, c.v)
		}
	}
}

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
