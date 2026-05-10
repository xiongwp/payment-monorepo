package meta

import "testing"

func TestBaseTableName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// V1 layout (2-digit suffix)
		{"payment_intents_00", "payment_intents"},
		{"payment_intents_42", "payment_intents"},
		{"payment_intents_99", "payment_intents"},
		{"charges_07", "charges"},

		// V1 + shadow
		{"payment_intents_42_shadow", "payment_intents"},
		{"charges_07_shadow", "charges"},

		// V2 layout (db_NN + tbl_NNN)
		{"voucher_03_007", "voucher"},
		{"voucher_99_999", "voucher"},

		// V2 + shadow
		{"voucher_07_813_shadow", "voucher"},

		// 无 shard 后缀（meta 表 / lookup 表）
		{"leaf_alloc", "leaf_alloc"},
		{"merchants", "merchants"},
		{"merchant_secret", "merchant_secret"},

		// 边界：表名末尾是数字但不带 _ 分隔
		{"audit2024", "audit2024"},

		// 表名含数字段（不是 shard 后缀）：例如 v2 / api 版本号
		{"webhook_v2", "webhook_v2"},

		// 嵌套：_v2_xxx 不应被误剥
		{"webhook_v2_03", "webhook_v2"},
	}
	for _, c := range cases {
		got := baseTableName(c.in)
		if got != c.want {
			t.Errorf("baseTableName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
