package currency

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── IsSupported ──────────────────────────────────────────────────────────────

func TestIsSupported(t *testing.T) {
	supported := []string{"PHP", "USD", "EUR", "JPY", "KWD", "VND"}
	for _, code := range supported {
		assert.True(t, IsSupported(code), "expected %s to be supported", code)
	}
	unsupported := []string{"", "XXX", "BTC", "USDT", "usd"}
	for _, code := range unsupported {
		assert.False(t, IsSupported(code), "expected %s to be unsupported", code)
	}
}

// ─── Precision ────────────────────────────────────────────────────────────────

func TestPrecision(t *testing.T) {
	cases := []struct {
		code      string
		wantPrec  int
		wantError bool
	}{
		{"USD", 2, false},
		{"PHP", 2, false},
		{"JPY", 0, false},
		{"KRW", 0, false},
		{"KWD", 3, false},
		{"BHD", 3, false},
		{"XXX", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		prec, err := Precision(tc.code)
		if tc.wantError {
			assert.Error(t, err, "code=%s", tc.code)
		} else {
			require.NoError(t, err, "code=%s", tc.code)
			assert.Equal(t, tc.wantPrec, prec, "code=%s", tc.code)
		}
	}
}

// ─── StorageFactor ────────────────────────────────────────────────────────────

func TestStorageFactor(t *testing.T) {
	// prec=2  → 10^2 × 100 = 10000
	// prec=0  → 10^0 × 100 = 100
	// prec=3  → 10^3 × 100 = 100000
	cases := []struct {
		code       string
		wantFactor int64
		wantError  bool
	}{
		{"USD", 10000, false},
		{"PHP", 10000, false},
		{"JPY", 100, false},
		{"KRW", 100, false},
		{"KWD", 100000, false},
		{"XXX", 0, true},
	}
	for _, tc := range cases {
		f, err := StorageFactor(tc.code)
		if tc.wantError {
			assert.Error(t, err, "code=%s", tc.code)
		} else {
			require.NoError(t, err, "code=%s", tc.code)
			assert.Equal(t, tc.wantFactor, f, "code=%s", tc.code)
		}
	}
}

// ─── ToStorage ────────────────────────────────────────────────────────────────

func TestToStorage(t *testing.T) {
	cases := []struct {
		desc    string
		amount  string // decimal string
		code    string
		want    int64
		wantErr bool
	}{
		// USD (precision=2, factor=10000)
		{"USD $3.42", "3.42", "USD", 34200, false},
		{"USD $0.01", "0.01", "USD", 100, false},
		{"USD $0.00", "0.00", "USD", 0, false},
		{"USD $100.00", "100.00", "USD", 1000000, false},
		{"USD negative", "-1.50", "USD", -15000, false},

		// CNY (precision=2, factor=10000)
		{"CNY ¥1.00", "1.00", "PHP", 10000, false},
		{"CNY ¥0.10", "0.10", "PHP", 1000, false},

		// JPY (precision=0, factor=100)
		{"JPY ¥342", "342", "JPY", 34200, false},
		{"JPY ¥1", "1", "JPY", 100, false},
		{"JPY ¥0", "0", "JPY", 0, false},

		// KWD (precision=3, factor=100000)
		{"KWD 3.422", "3.422", "KWD", 342200, false},
		{"KWD 0.001", "0.001", "KWD", 100, false},

		// rounding: extra decimals beyond precision+2 are rounded
		{"USD sub-penny rounds", "3.421", "USD", 34210, false},

		// unknown currency
		{"unknown currency", "1.00", "XXX", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			d, _ := decimal.NewFromString(tc.amount)
			got, err := ToStorage(d, tc.code)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ─── FromStorage ──────────────────────────────────────────────────────────────

func TestFromStorage(t *testing.T) {
	cases := []struct {
		desc    string
		stored  int64
		code    string
		want    string // decimal string with ISO precision
		wantErr bool
	}{
		{"USD 34200 → 3.42", 34200, "USD", "3.42", false},
		{"USD 100 → 0.01", 100, "USD", "0.01", false},
		{"USD 0 → 0.00", 0, "USD", "0.00", false},
		{"USD 1000000 → 100.00", 1000000, "USD", "100.00", false},
		{"USD negative -15000 → -1.50", -15000, "USD", "-1.50", false},
		{"JPY 34200 → 342", 34200, "JPY", "342", false},
		{"JPY 100 → 1", 100, "JPY", "1", false},
		{"KWD 342200 → 3.422", 342200, "KWD", "3.422", false},
		{"unknown currency", 100, "XXX", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := FromStorage(tc.stored, tc.code)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			prec, _ := Precision(tc.code)
			assert.Equal(t, tc.want, got.StringFixed(int32(prec)))
		})
	}
}

// ─── FormatAmount ─────────────────────────────────────────────────────────────

func TestFormatAmount(t *testing.T) {
	cases := []struct {
		desc    string
		stored  int64
		code    string
		want    string
		wantErr bool
	}{
		{"USD 34200 → \"3.42\"", 34200, "USD", "3.42", false},
		{"USD 0 → \"0.00\"", 0, "USD", "0.00", false},
		{"USD 1000000 → \"100.00\"", 1000000, "USD", "100.00", false},
		{"JPY 34200 → \"342\"", 34200, "JPY", "342", false},
		{"KWD 342200 → \"3.422\"", 342200, "KWD", "3.422", false},
		{"unknown currency", 100, "XXX", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := FormatAmount(tc.stored, tc.code)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ─── Round-trip: ToStorage → FromStorage ─────────────────────────────────────

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		amount string
		code   string
	}{
		{"3.42", "USD"},
		{"0.01", "USD"},
		{"100.00", "PHP"},
		{"342", "JPY"},
		{"3.422", "KWD"},
		{"0.001", "KWD"},
	}
	for _, tc := range cases {
		t.Run(tc.amount+"_"+tc.code, func(t *testing.T) {
			d, _ := decimal.NewFromString(tc.amount)
			stored, err := ToStorage(d, tc.code)
			require.NoError(t, err)
			back, err := FromStorage(stored, tc.code)
			require.NoError(t, err)
			prec, _ := Precision(tc.code)
			assert.Equal(t, tc.amount, back.StringFixed(int32(prec)))
		})
	}
}

// ─── ToStorage consistency with StorageFactor ─────────────────────────────────

func TestToStorageConsistentWithFactor(t *testing.T) {
	// For whole-unit amounts, stored = amount_in_major_units × storageFactor
	for _, code := range []string{"USD", "JPY", "KWD", "PHP"} {
		f, err := StorageFactor(code)
		require.NoError(t, err)
		d := decimal.NewFromInt(1) // 1 major unit
		stored, err := ToStorage(d, code)
		require.NoError(t, err)
		assert.Equal(t, f, stored, "1 %s should store as its storage factor", code)
	}
}
