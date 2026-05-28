package sanction

import (
	"context"
	"testing"
)

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "abd", 1},          // 一次替换
		{"kitten", "sitting", 3},   // 经典例子
		{"flaw", "lawn", 2},        // 替换+替换
		{"", "abc", 3},             // 空 vs 非空
		{"abcd", "abdc", 2},        // 两次替换（不是 transposition）
	}
	for _, c := range cases {
		if got := Levenshtein(c.a, c.b); got != c.want {
			t.Errorf("Levenshtein(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFuzzyMatch(t *testing.T) {
	if !FuzzyMatch("Vladmir Putin", "Vladimir Putin", 2) {
		t.Error("typo within distance 2 should match")
	}
	if FuzzyMatch("John Smith", "Vladimir Putin", 2) {
		t.Error("totally different names should NOT match")
	}
	if !FuzzyMatch("OSAMA BIN LADEN", "osama bin laden", 0) {
		t.Error("case-insensitive exact should match with maxDist=0")
	}
}

func TestSimilarityRatio(t *testing.T) {
	if r := SimilarityRatio("foo", "foo"); r != 1.0 {
		t.Errorf("identical should be 1.0, got %v", r)
	}
	r := SimilarityRatio("kitten", "sitting")
	// 距离 3, maxLen 7 → 1 - 3/7 ≈ 0.571
	if r < 0.5 || r > 0.6 {
		t.Errorf("kitten/sitting similarity %v out of expected range", r)
	}
}

func TestCheckWithFuzzy_ExactStillFirst(t *testing.T) {
	s := loadTest()
	r := s.CheckWithFuzzy(context.Background(), "Vladimir Putin", "RU", 2)
	if r == nil {
		t.Fatal("exact should hit")
	}
	if r.Fuzzy {
		t.Errorf("exact match should NOT be flagged fuzzy")
	}
}

func TestCheckWithFuzzy_FallbackOnTypo(t *testing.T) {
	s := loadTest()
	// "Vladmir" 少一个 i —— exact 不命中，fuzzy 应该命中
	r := s.CheckWithFuzzy(context.Background(), "Vladmir Putin", "", 2)
	if r == nil {
		t.Fatal("fuzzy should hit on small typo")
	}
	if !r.Fuzzy {
		t.Error("typo hit should be flagged Fuzzy=true")
	}
	if r.Hits[0].UID != "EU-001" {
		t.Errorf("wrong fuzzy hit: %+v", r.Hits[0])
	}
}

func TestCheckWithFuzzy_NoMatchForRandom(t *testing.T) {
	s := loadTest()
	if r := s.CheckWithFuzzy(context.Background(), "Zxqwerty Mnoplop", "", 2); r != nil {
		t.Errorf("random name shouldn't fuzzy-match anything, got %+v", r)
	}
}

func TestMatchDOB(t *testing.T) {
	cases := []struct {
		q, t      string
		wantOK    bool
		wantLevel string
	}{
		{"1952-10-07", "07 Oct 1952", true, "exact"},
		{"1952-10-07", "1952-01-01", true, "year"},
		{"1952-10-07", "1980-01-01", false, ""},
		{"", "1952-10-07", true, "unknown"},
	}
	for _, c := range cases {
		ok, lvl := MatchDOB(c.q, c.t)
		if ok != c.wantOK || lvl != c.wantLevel {
			t.Errorf("MatchDOB(%q,%q)=(%v,%q) want (%v,%q)",
				c.q, c.t, ok, lvl, c.wantOK, c.wantLevel)
		}
	}
}

func TestParseDOB_Formats(t *testing.T) {
	inputs := []string{"1952-10-07", "07 Oct 1952", "1952", "2006/01/02"}
	for _, in := range inputs {
		if _, err := ParseDOB(in); err != nil {
			t.Errorf("ParseDOB(%q) failed: %v", in, err)
		}
	}
	if _, err := ParseDOB(""); err == nil {
		t.Error("empty DOB should error")
	}
	if _, err := ParseDOB("not-a-date"); err == nil {
		t.Error("garbage DOB should error")
	}
}
