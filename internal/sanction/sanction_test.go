package sanction

import (
	"context"
	"strings"
	"testing"
)

func loadTest() *MemService {
	s := NewMemService()
	_ = s.Reload(context.Background(), []*Entry{
		{Source: SourceOFAC, UID: "12345", Name: "OSAMA BIN LADEN", Country: "SA",
			Aliases: []string{"USAMA BIN LADEN", "Bin Laden, Osama"}},
		{Source: SourceEU, UID: "EU-001", Name: "VLADIMIR PUTIN", Country: "RU"},
		{Source: SourceUN, UID: "UN-99", Name: "Abu Bakr al-Baghdadi", Country: "IQ",
			Aliases: []string{"Ibrahim Awad Ibrahim al-Badri"}},
		{Source: SourceOFAC, UID: "33333", Name: "ACME EVIL CORP", Type: "entity"},
	})
	return s
}

func TestCheck_ExactName(t *testing.T) {
	s := loadTest()
	m := s.Check(context.Background(), "Osama Bin Laden", "")
	if m == nil || len(m.Hits) == 0 {
		t.Fatal("expected hit on exact name")
	}
	if m.Hits[0].UID != "12345" {
		t.Fatalf("wrong hit: %v", m.Hits[0])
	}
}

func TestCheck_AliasMatch(t *testing.T) {
	s := loadTest()
	m := s.Check(context.Background(), "USAMA BIN LADEN", "")
	if m == nil || m.Hits[0].UID != "12345" {
		t.Fatalf("alias should match, got %+v", m)
	}
}

func TestCheck_TokenBagOrder(t *testing.T) {
	s := loadTest()
	// "Bin Laden Osama" 顺序乱 → tokens 都在某 entry 里 → 命中
	m := s.Check(context.Background(), "Bin Laden Osama", "")
	if m == nil || m.Hits[0].UID != "12345" {
		t.Fatalf("token-bag match failed: %+v", m)
	}
}

func TestCheck_NoCrossCountryFalsePositive(t *testing.T) {
	s := loadTest()
	// 同名但不同国家应跳过
	m := s.Check(context.Background(), "Vladimir Putin", "US")
	if m != nil {
		t.Fatalf("country mismatch should not hit, got %+v", m)
	}
	m = s.Check(context.Background(), "Vladimir Putin", "RU")
	if m == nil || m.Hits[0].UID != "EU-001" {
		t.Fatalf("same country should hit: %+v", m)
	}
}

func TestCheck_MissesUnknown(t *testing.T) {
	s := loadTest()
	if m := s.Check(context.Background(), "John Smith", ""); m != nil {
		t.Fatalf("clean name should not hit, got %+v", m)
	}
}

func TestCheck_PunctuationStripped(t *testing.T) {
	s := loadTest()
	m := s.Check(context.Background(), "Bin Laden, Osama", "")
	if m == nil {
		t.Fatal("comma form should still match")
	}
}

func TestCheck_EntityName(t *testing.T) {
	s := loadTest()
	if m := s.Check(context.Background(), "  Acme  Evil   Corp  ", ""); m == nil || m.Hits[0].UID != "33333" {
		t.Fatalf("entity should match, got %+v", m)
	}
}

func TestParseCSV(t *testing.T) {
	csv := `ofac_sdn,1,KIM JONG UN,KIM JONG-UN|Kim 3,individual,KP,1984-01-08,DPRK1
eu_consolidated,2,SOME ENTITY,,entity,,,EU-RU`
	entries, err := parseCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2, got %d", len(entries))
	}
	if entries[0].UID != "1" || len(entries[0].Aliases) != 2 || entries[0].BirthDate != "1984-01-08" {
		t.Fatalf("entry[0] parse: %+v", entries[0])
	}
	if entries[1].Source != SourceEU || entries[1].Type != "entity" {
		t.Fatalf("entry[1] parse: %+v", entries[1])
	}
}

func TestStats(t *testing.T) {
	s := loadTest()
	n, ts := s.Stats()
	if n != 4 {
		t.Fatalf("expected 4 entries, got %d", n)
	}
	if ts.IsZero() {
		t.Fatal("lastReloadAt should be set")
	}
}
