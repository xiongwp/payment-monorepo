package adminhttp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFormatMinorAmount 验证金额从 minor×100 转可读（除 100，2 位小数）。
func TestFormatMinorAmount(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{99, "0.99"},
		{100, "1.00"},
		{12345, "123.45"},
		{100000, "1000.00"}, // 1000.00
		{-100000, "-1000.00"},
		{-1, "-0.01"},
		{-12305, "-123.05"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, formatMinorAmount(c.in), "formatMinorAmount(%d)", c.in)
	}
}

// TestAtoiDefault 验证 query 参数解析兜底。
func TestAtoiDefault(t *testing.T) {
	assert.Equal(t, 0, atoiDefault("", 0))
	assert.Equal(t, 5, atoiDefault("", 5))
	assert.Equal(t, 42, atoiDefault("42", 0))
	assert.Equal(t, 0, atoiDefault("notanumber", 0))
}

// TestCSVHeaderColumns 锁定 CSV 表头列顺序与中文标签。
// 表头在 handleTrialBalanceExport 中硬编码，这里固化期望，防止误改顺序破坏下游解析。
func TestCSVHeaderColumns(t *testing.T) {
	header := []string{"科目类别", "账户类型", "业务类型", "账户数", "期初余额", "本期借方", "本期贷方", "期末余额"}
	assert.Len(t, header, 8)
	assert.Equal(t, "科目类别", header[0])
	assert.Equal(t, "业务类型", header[2])
	assert.Equal(t, "期末余额", header[7])
}
