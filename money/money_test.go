package money

import "testing"

func TestMinorToStorage(t *testing.T) {
	cases := []struct {
		name    string
		minor   int64
		code    string
		storage int64
	}{
		{"PHP 100 cents = 1 PHP", 100, "PHP", 10000},
		{"PHP 10000 cents = 100 PHP", 10000, "PHP", 1000000},
		{"JPY 100 yen", 100, "JPY", 10000},
		{"KWD 1000 fils = 1 dinar", 1000, "KWD", 100000},
		{"USD 525 cents", 525, "USD", 52500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := MinorToStorage(c.minor, c.code)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.storage {
				t.Errorf("got %d, want %d", got, c.storage)
			}
			// 回路：StorageToMinor 应该还原
			back, err := StorageToMinor(got, c.code)
			if err != nil || back != c.minor {
				t.Errorf("round trip: got %d err=%v, want %d", back, err, c.minor)
			}
		})
	}
}

func TestUnknownCurrency(t *testing.T) {
	if _, err := MinorToStorage(100, "ZZZ"); err == nil {
		t.Errorf("expected error for unknown currency")
	}
}

func TestMinorUnitsPerMajor(t *testing.T) {
	cases := map[string]int64{
		"PHP": 100,
		"JPY": 1,
		"KWD": 1000,
		"CLF": 10000, // 4 位精度
	}
	for code, want := range cases {
		got, err := MinorUnitsPerMajor(code)
		if err != nil {
			t.Fatalf("%s err: %v", code, err)
		}
		if got != want {
			t.Errorf("%s: got %d, want %d", code, got, want)
		}
	}
}

func TestStorageToMinorStrict(t *testing.T) {
	if _, err := StorageToMinor(1003460, "USD"); err == nil {
		t.Errorf("expected error for non-divisible storage")
	}
	got, err := StorageToMinor(1003400, "USD")
	if err != nil || got != 10034 {
		t.Errorf("got %d err=%v, want 10034 nil", got, err)
	}
}

func TestStorageToMinorBanker(t *testing.T) {
	cases := []struct {
		name    string
		storage int64
		code    string
		want    int64
	}{
		{"USD .4 down", 1003440, "USD", 10034},
		{"USD .6 up", 1003460, "USD", 10035},
		{"USD .5 → even (already even)", 1003450, "USD", 10034},
		{"USD .5 → even (round up)", 1003550, "USD", 10036},
		{"USD .5 → even small", 50, "USD", 0},                     // 0.5 → 0 (even)
		{"USD .5 → even small odd q", 150, "USD", 2},              // 1.5 → 2 (even)
		{"USD .5 → even small odd q 2", 250, "USD", 2},            // 2.5 → 2 (even)
		{"USD .5 → even small odd q 3", 350, "USD", 4},            // 3.5 → 4 (even)
		{"USD negative .6", -1003460, "USD", -10035},
		{"USD negative .5 even", -1003450, "USD", -10034},
		{"JPY exact", 1000, "JPY", 10},
		{"KWD exact", 100000, "KWD", 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := StorageToMinorBanker(c.storage, c.code)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestFormatMinor(t *testing.T) {
	cases := []struct {
		minor int64
		code  string
		want  string
	}{
		{10034, "USD", "100.34"},
		{0, "USD", "0.00"},
		{5, "USD", "0.05"},
		{-25, "USD", "-0.25"},
		{100, "JPY", "100"},
		{0, "JPY", "0"},
		{-100, "JPY", "-100"},
		{1500, "KWD", "1.500"},
		{1, "KWD", "0.001"},
		{12345, "CLF", "1.2345"},
	}
	for _, c := range cases {
		got, err := FormatMinor(c.minor, c.code)
		if err != nil {
			t.Fatalf("%s %d err: %v", c.code, c.minor, err)
		}
		if got != c.want {
			t.Errorf("%s %d: got %q, want %q", c.code, c.minor, got, c.want)
		}
	}
}

func TestDisplay(t *testing.T) {
	cases := []struct {
		storage int64
		code    string
		want    string
	}{
		// 用户给的范例
		{1003400, "USD", "$100.34"},
		{1003460, "USD", "$100.35"}, // banker round up (.6)
		{1003450, "USD", "$100.34"}, // banker .5 → even (4)
		{1003550, "USD", "$100.36"}, // banker .5 → even (6)
		{10000, "JPY", "¥100"},
		{150000, "KWD", "KD1.000"},
		{1000000, "PHP", "₱100.00"},
		{100000, "EUR", "€10.00"},
		// 未登记币种走 fallback
		{10000, "ZZZ", "ZZZ "},
	}
	for _, c := range cases {
		got, err := Display(c.storage, c.code)
		if c.code == "ZZZ" {
			if err == nil {
				t.Errorf("ZZZ expected err")
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s %d err: %v", c.code, c.storage, err)
		}
		if got != c.want {
			t.Errorf("%s %d: got %q, want %q", c.code, c.storage, got, c.want)
		}
	}
}

func TestSymbolFallback(t *testing.T) {
	// 没在 currencySymbol map 里登记的，应回落到 "<CODE> "
	if got := Symbol("XYZ"); got != "XYZ " {
		t.Errorf("Symbol XYZ = %q, want \"XYZ \"", got)
	}
	if got := Symbol("USD"); got != "$" {
		t.Errorf("Symbol USD = %q, want $", got)
	}
}

func TestISO4217Coverage(t *testing.T) {
	// 抽查若干代表性币种确实登记了
	for _, code := range []string{
		"USD", "EUR", "GBP", "JPY", "CNY", "PHP", "INR", "BRL",
		"KRW", "VND", "IDR", "THB", "TRY", "RUB", "SAR", "AED",
		"BHD", "KWD", "OMR", "JOD", "TND",
		"CLF", "UYW",
		"XAF", "XOF", "XPF", "XDR",
	} {
		if !IsSupported(code) {
			t.Errorf("expected %s to be supported", code)
		}
	}
}
