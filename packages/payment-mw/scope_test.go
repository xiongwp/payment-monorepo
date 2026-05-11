package mw

import "testing"

func TestScopeMatches(t *testing.T) {
	cases := []struct {
		granted, want string
		expect        bool
	}{
		// 严格
		{"refund:write", "refund:write", true},
		{"refund:read", "refund:write", false},

		// 通配 resource
		{"refund:*", "refund:write", true},
		{"refund:*", "refund:read", true},
		{"refund:*", "charge:write", false},

		// 通配 action
		{"*:read", "refund:read", true},
		{"*:read", "charge:read", true},
		{"*:read", "refund:write", false},

		// 全通配
		{"*", "anything:write", true},
		{"*:*", "refund:write", true},

		// write 隐含 read
		{"refund:write", "refund:read", true},
		{"refund:write", "charge:read", false}, // 不同 resource
		{"refund:read", "refund:write", false}, // read 不隐含 write

		// 非法格式
		{"weird", "refund:write", false},
		{"refund:write", "noflag", false},
	}
	for _, c := range cases {
		got := scopeMatches(c.granted, c.want)
		if got != c.expect {
			t.Errorf("scopeMatches(%q, %q) = %v, want %v", c.granted, c.want, got, c.expect)
		}
	}
}
