package cohort

import (
	"testing"
	"time"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/feedback"
)

func mkA(id, merchant, country, verdict string) *audit.DecisionAudit {
	return &audit.DecisionAudit{
		DecisionID: id,
		OccurredAt: time.Now(),
		Verdict:    verdict,
		Input: audit.AuditInput{
			MerchantID: merchant,
			Country:    country,
		},
	}
}

func TestCompute_GroupByMerchant_AggregatesCounts(t *testing.T) {
	audits := []*audit.DecisionAudit{
		mkA("d1", "m_a", "US", "ALLOW"),
		mkA("d2", "m_a", "US", "DENY"),
		mkA("d3", "m_a", "US", "REVIEW"),
		mkA("d4", "m_b", "GB", "ALLOW"),
	}
	got := Compute(audits, nil, GroupByMerchant, 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 merchants; got %d", len(got))
	}
	// m_a 在前（Total 大）
	if got[0].Key != "m_a" || got[0].Total != 3 || got[0].Block != 2 || got[0].Allow != 1 {
		t.Fatalf("m_a wrong: %+v", got[0])
	}
	if got[0].BlockRate < 0.66 || got[0].BlockRate > 0.67 {
		t.Fatalf("m_a block_rate ~0.667; got %.3f", got[0].BlockRate)
	}
}

func TestCompute_PrecisionAtBlock_NeedsMinLabels(t *testing.T) {
	rec := feedback.NewMemRecorder(0)
	// m_a 6 笔 DENY，5 个有 label：4 fraud + 1 legit → precision = 0.8
	audits := []*audit.DecisionAudit{}
	for i := 0; i < 6; i++ {
		id := "p" + string(rune('0'+i))
		audits = append(audits, mkA(id, "m_a", "US", "DENY"))
		if i < 5 {
			_ = rec.Record(feedback.Outcome{
				DecisionID: id, Source: feedback.SourceDispute, IsFraud: i < 4,
			})
		}
	}
	got := Compute(audits, rec, GroupByMerchant, 0)
	s := got[0]
	if s.BlockedFraud != 4 || s.BlockedLegit != 1 {
		t.Fatalf("expected 4 TP / 1 FP; got fraud=%d legit=%d", s.BlockedFraud, s.BlockedLegit)
	}
	if s.PrecisionAtBlock < 0.79 || s.PrecisionAtBlock > 0.81 {
		t.Fatalf("precision ~0.8; got %.3f", s.PrecisionAtBlock)
	}
}

func TestCompute_LowSampleNoPrecision(t *testing.T) {
	rec := feedback.NewMemRecorder(0)
	// 3 笔 block 全部 fraud；< 5 不该算 precision
	audits := []*audit.DecisionAudit{
		mkA("a", "m_x", "US", "DENY"),
		mkA("b", "m_x", "US", "DENY"),
		mkA("c", "m_x", "US", "DENY"),
	}
	for _, did := range []string{"a", "b", "c"} {
		_ = rec.Record(feedback.Outcome{DecisionID: did, Source: feedback.SourceDispute, IsFraud: true})
	}
	got := Compute(audits, rec, GroupByMerchant, 0)
	if got[0].PrecisionAtBlock != 0 {
		t.Fatalf("low-N should have precision=0; got %.3f", got[0].PrecisionAtBlock)
	}
}

func TestCompute_GroupByCountry_NormalizesToUpper(t *testing.T) {
	audits := []*audit.DecisionAudit{
		mkA("d1", "m1", "us", "ALLOW"),
		mkA("d2", "m2", "US", "DENY"),
	}
	got := Compute(audits, nil, GroupByCountry, 0)
	if len(got) != 1 || got[0].Key != "US" {
		t.Fatalf("expected single key=US; got %+v", got)
	}
	if got[0].Total != 2 {
		t.Fatalf("expected total=2; got %d", got[0].Total)
	}
}

func TestCompute_MinTotalFilter(t *testing.T) {
	audits := []*audit.DecisionAudit{
		mkA("d1", "m_big", "US", "ALLOW"),
		mkA("d2", "m_big", "US", "ALLOW"),
		mkA("d3", "m_big", "US", "DENY"),
		mkA("d4", "m_tiny", "GB", "ALLOW"), // 单条，会被 minTotal=2 过滤掉
	}
	got := Compute(audits, nil, GroupByMerchant, 2)
	if len(got) != 1 || got[0].Key != "m_big" {
		t.Fatalf("expected only m_big; got %+v", got)
	}
}

func TestCompute_EmptyKeySkipped(t *testing.T) {
	audits := []*audit.DecisionAudit{
		mkA("d1", "", "US", "ALLOW"),
		mkA("d2", "m_real", "US", "ALLOW"),
	}
	got := Compute(audits, nil, GroupByMerchant, 0)
	if len(got) != 1 || got[0].Key != "m_real" {
		t.Fatalf("empty key should be skipped; got %+v", got)
	}
}

func TestCompute_NilAuditsAndRecorder(t *testing.T) {
	got := Compute(nil, nil, GroupByMerchant, 0)
	if len(got) != 0 {
		t.Fatalf("nil input should return empty; got %d", len(got))
	}
}

func mkAt(id, merchant, verdict string, when time.Time) *audit.DecisionAudit {
	return &audit.DecisionAudit{
		DecisionID: id,
		OccurredAt: when,
		Verdict:    verdict,
		Input:      audit.AuditInput{MerchantID: merchant},
	}
}

func TestTimeSeries_DayBucket(t *testing.T) {
	d1 := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 4, 27, 23, 30, 0, 0, time.UTC) // 同一天
	d3 := time.Date(2026, 4, 28, 1, 0, 0, 0, time.UTC)   // 第二天
	audits := []*audit.DecisionAudit{
		mkAt("a", "m", "ALLOW", d1),
		mkAt("b", "m", "DENY", d2),
		mkAt("c", "m", "ALLOW", d3),
	}
	got := TimeSeries(audits, nil, GroupByMerchant, "", BucketDay)
	if len(got) != 2 {
		t.Fatalf("expected 2 day buckets; got %d (%+v)", len(got), got)
	}
	// 升序：27 在前，28 在后
	if !got[0].BucketStart.Before(got[1].BucketStart) {
		t.Fatal("buckets should be ascending")
	}
	if got[0].Total != 2 || got[0].Block != 1 {
		t.Fatalf("day 27 wrong: %+v", got[0])
	}
	if got[1].Total != 1 || got[1].Block != 0 {
		t.Fatalf("day 28 wrong: %+v", got[1])
	}
}

func TestTimeSeries_HourBucket(t *testing.T) {
	t1 := time.Date(2026, 4, 27, 10, 5, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 27, 10, 59, 0, 0, time.UTC)
	t3 := time.Date(2026, 4, 27, 11, 0, 0, 0, time.UTC)
	audits := []*audit.DecisionAudit{
		mkAt("a", "m", "ALLOW", t1),
		mkAt("b", "m", "ALLOW", t2),
		mkAt("c", "m", "DENY", t3),
	}
	got := TimeSeries(audits, nil, GroupByMerchant, "", BucketHour)
	if len(got) != 2 {
		t.Fatalf("expected 2 hour buckets; got %d", len(got))
	}
	if got[0].Total != 2 || got[1].Total != 1 {
		t.Fatalf("hour bucketing wrong: %+v", got)
	}
}

func TestTimeSeries_WeekBucketTruncatesToMonday(t *testing.T) {
	// 2026-04-29 是周三；应回退到 2026-04-27（周一）
	wed := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	audits := []*audit.DecisionAudit{
		mkAt("a", "m", "ALLOW", wed),
	}
	got := TimeSeries(audits, nil, GroupByMerchant, "", BucketWeek)
	if len(got) != 1 {
		t.Fatalf("expected 1 week bucket; got %d", len(got))
	}
	expected := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	if !got[0].BucketStart.Equal(expected) {
		t.Fatalf("expected bucket start=%s; got %s", expected, got[0].BucketStart)
	}
}

func TestTimeSeries_OptionalKeyFiltersToCohort(t *testing.T) {
	when := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	audits := []*audit.DecisionAudit{
		mkAt("a", "m_a", "ALLOW", when),
		mkAt("b", "m_b", "ALLOW", when),
		mkAt("c", "m_a", "DENY", when),
	}
	got := TimeSeries(audits, nil, GroupByMerchant, "m_a", BucketDay)
	if len(got) != 1 {
		t.Fatalf("expected 1 bucket; got %d", len(got))
	}
	if got[0].Total != 2 || got[0].Block != 1 {
		t.Fatalf("expected total=2 block=1 for m_a only; got %+v", got[0])
	}
}

func TestTimeSeries_FraudRateWithFeedback(t *testing.T) {
	rec := feedback.NewMemRecorder(0)
	when := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	audits := []*audit.DecisionAudit{
		mkAt("d1", "m", "DENY", when),
		mkAt("d2", "m", "DENY", when),
		mkAt("d3", "m", "ALLOW", when),
	}
	_ = rec.Record(feedback.Outcome{DecisionID: "d1", Source: feedback.SourceDispute, IsFraud: true})
	_ = rec.Record(feedback.Outcome{DecisionID: "d2", Source: feedback.SourceDispute, IsFraud: false})
	got := TimeSeries(audits, rec, GroupByMerchant, "", BucketDay)
	if got[0].LabeledTotal != 2 || got[0].ActualFraud != 1 {
		t.Fatalf("expected 2 labeled / 1 fraud; got %+v", got[0])
	}
	if got[0].ActualFraudRate < 0.49 || got[0].ActualFraudRate > 0.51 {
		t.Fatalf("expected fraud_rate ~0.5; got %.3f", got[0].ActualFraudRate)
	}
}

func TestTimeSeries_NilInputs(t *testing.T) {
	got := TimeSeries(nil, nil, GroupByMerchant, "", BucketDay)
	if len(got) != 0 {
		t.Fatalf("expected empty; got %d", len(got))
	}
}
