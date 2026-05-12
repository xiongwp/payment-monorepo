package rails

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func testOrig() Originator {
	return Originator{
		Name: "Payment Platform Inc",
		ID:   "EIN1234567",
		Account: BankAccount{
			HolderName:    "Payment Platform Inc",
			AccountNumber: "0123456789",
			RoutingNum:    "121000358",
			IBAN:          "DE89370400440532013000",
			BIC:           "DEUTDEFFXXX",
			Country:       "US",
		},
	}
}

func TestNACHA_GeneratesValidStructure(t *testing.T) {
	payouts := []Payout{{
		PayoutID:    "po_1",
		Description: "test payout",
		AmountCents: 100_00,
		Currency:    "USD",
		ValueDate:   time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC),
	}}
	accounts := []BankAccount{{
		HolderName:    "Merchant ACME",
		AccountNumber: "9876543210",
		RoutingNum:    "021000021",
		Country:       "US",
	}}
	out, err := GenerateNACHA(testOrig(), payouts, accounts, "A")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	// 至少 5 行: file_hdr + batch_hdr + entry + batch_ctrl + file_ctrl, 可能 + 9 pad
	if len(lines) < 5 {
		t.Fatalf("want >=5 lines, got %d", len(lines))
	}
	// 第 1 行 type 1
	if lines[0][0] != '1' {
		t.Errorf("file header first char = %q, want '1'", lines[0][0])
	}
	// 每行 94 char
	for i, l := range lines {
		if len(l) != 94 {
			t.Errorf("line %d len = %d, want 94", i, len(l))
		}
	}
	// 最后一行 (或 9-padded 之前) 应是 type 9 file ctrl
	hasFileCtrl := false
	for _, l := range lines {
		if l[0] == '9' && !strings.HasPrefix(l, strings.Repeat("9", 94)) {
			hasFileCtrl = true
			break
		}
	}
	if !hasFileCtrl {
		t.Error("no file control record found")
	}
}

func TestSEPA_GeneratesValidXML(t *testing.T) {
	payouts := []Payout{{
		PayoutID:    "po_eur_1",
		Description: "Invoice 0001",
		AmountCents: 1500_00,
		Currency:    "EUR",
		EndToEndID:  "E2E-001",
		ValueDate:   time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC),
	}}
	accounts := []BankAccount{{
		HolderName: "Acme GmbH",
		IBAN:       "DE89 3704 0044 0532 0130 00",
		BIC:        "COBADEFFXXX",
		Country:    "DE",
	}}
	out, err := GenerateSEPA(testOrig(), payouts, accounts)
	if err != nil {
		t.Fatal(err)
	}
	// xml 可解析
	var probe struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("xml invalid: %v\n%s", err, out)
	}
	// IBAN 应已 strip 空格
	if !strings.Contains(string(out), "DE89370400440532013000") {
		t.Error("IBAN not stripped of spaces")
	}
	if !strings.Contains(string(out), "<NbOfTxs>1</NbOfTxs>") {
		t.Error("missing tx count")
	}
	if !strings.Contains(string(out), `Ccy="EUR"`) {
		t.Error("currency not EUR")
	}
}

func TestSEPA_RejectsNonEUR(t *testing.T) {
	payouts := []Payout{{Currency: "USD", AmountCents: 100}}
	accounts := []BankAccount{{}}
	_, err := GenerateSEPA(testOrig(), payouts, accounts)
	if err == nil {
		t.Error("expected error for non-EUR SEPA")
	}
}

func TestMT103_HasRequiredFields(t *testing.T) {
	p := Payout{
		OurRef:      "MYREF12345",
		Description: "Cross-border payment for invoice",
		AmountCents: 5_000_00,
		Currency:    "USD",
		ValueDate:   time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC),
	}
	ben := BankAccount{
		HolderName:    "International Corp",
		AccountNumber: "12345678",
		BIC:           "CHASUS33XXX",
		Country:       "US",
	}
	out, err := GenerateMT103(testOrig(), p, ben, ChargeSHA)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// 必要字段
	for _, tag := range []string{":20:MYREF12345", ":23B:CRED", ":32A:260515USD5000,00", ":57A:CHASUS33XXX", ":71A:SHA"} {
		if !strings.Contains(s, tag) {
			t.Errorf("missing field %q in MT103", tag)
		}
	}
}

func TestDispatcher_SelectsNetwork(t *testing.T) {
	cases := []struct {
		ccy, country string
		want         Network
	}{
		{"USD", "US", NetworkACH},
		{"EUR", "DE", NetworkSEPA},
		{"EUR", "FR", NetworkSEPA},
		{"GBP", "GB", NetworkUKFaster},
		{"USD", "SG", NetworkWireSWIFT},
		{"USD", "DE", NetworkWireSWIFT}, // cross-currency
	}
	for _, c := range cases {
		got := SelectNetworkFor(c.ccy, c.country)
		if got != c.want {
			t.Errorf("Select(%s, %s) = %s, want %s", c.ccy, c.country, got, c.want)
		}
	}
}
