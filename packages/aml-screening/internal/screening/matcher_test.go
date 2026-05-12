package screening

import (
	"testing"
	"time"

	"reconcile-system/packages/aml-screening/internal/domain"
)

func TestJaroWinkler(t *testing.T) {
	cases := []struct {
		a, b string
		min  float64
	}{
		{"john doe", "john doe", 1.0},
		{"john doe", "jon doe", 0.9},        // 单字符 typo
		{"john smith", "jon smyth", 0.85},    // 多字符 typo
		{"hello", "world", 0},                // 不相关
	}
	for _, c := range cases {
		got := JaroWinkler(c.a, c.b)
		if got < c.min {
			t.Errorf("JW(%q,%q) = %.2f, want >= %.2f", c.a, c.b, got, c.min)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"John Smith", "john smith"},
		{"SMITH, John", "john smith"},                // 词序无关
		{"John Smith Inc.", "john smith"},            // 公司后缀剥离
		{"BMW AG Holdings", "bmw"},                   // 反复剥离
		{"José Martínez", "jose martinez"},           // 重音
		{"O'Neill", "oneill"},                        // 标点
	}
	for _, c := range cases {
		got := NormalizeName(c.in)
		if got != c.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatcher_StrongHit_OnSanctionedName(t *testing.T) {
	cfg := DefaultConfig()
	now := time.Now()
	entries := []domain.ListEntry{
		{
			ID:          "OFAC-1",
			Source:      domain.SourceOFACSDN,
			PrimaryName: "John Doe",
			Aliases:     []string{"Johnny Doe"},
			DOB:         "1970-01-01",
			Nationality: []string{"IR"},
			UpdatedAt:   now,
		},
	}
	req := domain.ScreenRequest{
		Name:        "John Doe",
		DOB:         "1970-01-01",
		Nationality: "Iran",
	}
	hits := Match(req, entries, cfg)
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if hits[0].Confidence < 80 {
		t.Errorf("confidence = %d, want >= 80", hits[0].Confidence)
	}
	action, _ := Decide(hits, cfg)
	if action != domain.ActionBlock && action != domain.ActionReview {
		t.Errorf("action = %s, want block/review", action)
	}
}

func TestMatcher_FuzzyName_TriggersReview(t *testing.T) {
	cfg := DefaultConfig()
	entries := []domain.ListEntry{
		{
			ID:          "OFAC-2",
			Source:      domain.SourceOFACSDN,
			PrimaryName: "John Doe",
		},
	}
	// 略有 typo
	req := domain.ScreenRequest{Name: "Jon Doe"}
	hits := Match(req, entries, cfg)
	if len(hits) == 0 {
		t.Fatal("want at least 1 fuzzy hit")
	}
}

func TestMatcher_NoHit_RandomName(t *testing.T) {
	cfg := DefaultConfig()
	entries := []domain.ListEntry{
		{ID: "X", PrimaryName: "Sanctioned Person"},
	}
	req := domain.ScreenRequest{Name: "Random Unknown Citizen"}
	hits := Match(req, entries, cfg)
	if len(hits) != 0 {
		t.Errorf("want 0 hits for unrelated name, got %d", len(hits))
	}
	action, _ := Decide(hits, cfg)
	if action != domain.ActionPass {
		t.Errorf("want pass, got %s", action)
	}
}
