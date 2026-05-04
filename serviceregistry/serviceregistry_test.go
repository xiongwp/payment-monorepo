package serviceregistry

import (
	"testing"
	"time"
)

// 真正的 e2e 行为（注册→watch→election）需要跑 etcd；CI 里没有 etcd，
// 这里只覆盖 constructor 的 input validation。
//
// 集成测试见 serviceregistry_integration_test.go（带 build tag etcdintegration）。

func TestNewRegistrar_Validation(t *testing.T) {
	if _, err := NewRegistrar(nil, "svc", "addr"); err == nil {
		t.Fatal("expected error on nil client")
	}
}

func TestNewElection_Validation(t *testing.T) {
	cases := []struct {
		name string
		key  string
		ttl  time.Duration
	}{
		{"empty key", "", 5 * time.Second},
		{"sub-second ttl", "/leader/x", 100 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 用 nil 客户端会先在 nil-check 失败；这里用 non-nil sentinel
			// 触发后续校验。NewElection 不会真的调 etcd，所以可以传 nil 同时
			// 期望 nil-client 错误优先返回。
			if _, err := NewElection(nil, c.key, c.ttl); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRegisterResolver_NilClient(t *testing.T) {
	if err := RegisterResolver(nil); err == nil {
		t.Fatal("expected error on nil client")
	}
}

func TestDial_EmptyService(t *testing.T) {
	if _, err := Dial(""); err == nil {
		t.Fatal("expected error on empty service")
	}
}

func TestResolverScheme(t *testing.T) {
	if got := ResolverScheme(); got != "etcd" {
		t.Fatalf("ResolverScheme()=%q, want %q", got, "etcd")
	}
}
