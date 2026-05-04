package maya

import (
	"encoding/json"
	"testing"
)

// TestMinorAmount_MarshalExact 关键回归：旧实现 float64(10001)/100 = 100.0099999...
// 序列化出去会被 Maya 严格校验拒；新实现直接走 int64 → "100.01" 字面量。
func TestMinorAmount_MarshalExact(t *testing.T) {
	cases := []struct {
		in   minorAmount
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{99, "0.99"},
		{100, "1.00"},
		{10001, "100.01"},   // 旧 float 实现会丢精度
		{12345, "123.45"},
		{1_000_000_00, "1000000.00"},
		{-100, "-1.00"},
		{-10001, "-100.01"},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.in)
		if err != nil {
			t.Errorf("Marshal(%d) err: %v", c.in, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("Marshal(%d) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestMinorAmount_RoundTrip 通过 mayaAmount 完整 marshal 一次，确认 JSON
// 形态符合 Maya API 期望（value 是 number 而非 string，currency 是 string）。
func TestMinorAmount_RoundTrip(t *testing.T) {
	a := mayaAmount{Value: 12345, Currency: "PHP"}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"value":123.45,"currency":"PHP"}`
	if string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
}
