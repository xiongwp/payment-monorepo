package auth

import (
	"context"
	"errors"
	"testing"
)

func TestMemStore_AddLookupRevoke(t *testing.T) {
	s := NewMemAPIKeyStore()
	tok := "rsk_live_m1_abcdef"
	s.Add(tok, Principal{Scope: ScopeMerchant, MerchantID: "m1"})

	p, err := s.Lookup(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope != ScopeMerchant || p.MerchantID != "m1" {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if p.KeyID == "" {
		t.Fatal("KeyID should be auto-filled")
	}

	s.Revoke(tok)
	if _, err := s.Lookup(context.Background(), tok); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("revoked key should fail with ErrInvalidKey, got %v", err)
	}
}

func TestMemStore_LookupUnknownKey(t *testing.T) {
	s := NewMemAPIKeyStore()
	_, err := s.Lookup(context.Background(), "rsk_live_m1_nosuchkey")
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("unknown key should fail with ErrInvalidKey, got %v", err)
	}
}

func TestRequireMerchantMatch(t *testing.T) {
	cases := []struct {
		name        string
		principal   *Principal
		claimed     string
		expectError bool
	}{
		{"no principal = dev", nil, "m1", false},
		{"admin scope = no enforcement", &Principal{Scope: ScopeAdmin}, "m1", false},
		{"internal scope = no enforcement", &Principal{Scope: ScopeInternal}, "m1", false},
		{"merchant matches", &Principal{Scope: ScopeMerchant, MerchantID: "m1"}, "m1", false},
		{"merchant mismatch", &Principal{Scope: ScopeMerchant, MerchantID: "m1"}, "m2", true},
		{"merchant empty claimed = autofill", &Principal{Scope: ScopeMerchant, MerchantID: "m1"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.principal != nil {
				ctx = WithPrincipal(ctx, tc.principal)
			}
			err := RequireMerchantMatch(ctx, tc.claimed)
			if (err != nil) != tc.expectError {
				t.Fatalf("expected error=%v, got %v", tc.expectError, err)
			}
		})
	}
}

func TestFillMerchantID(t *testing.T) {
	ctx := WithPrincipal(context.Background(), &Principal{Scope: ScopeMerchant, MerchantID: "m1"})
	if got := FillMerchantID(ctx, ""); got != "m1" {
		t.Fatalf("merchant scope empty claim should fill m1, got %q", got)
	}
	if got := FillMerchantID(ctx, "m_explicit"); got != "m_explicit" {
		t.Fatalf("explicit claim should win, got %q", got)
	}
	// admin scope shouldn't autofill
	ctx = WithPrincipal(context.Background(), &Principal{Scope: ScopeAdmin})
	if got := FillMerchantID(ctx, ""); got != "" {
		t.Fatalf("admin empty claim should stay empty, got %q", got)
	}
}

func TestAllowedMerchant(t *testing.T) {
	// merchant scope: forced to own merchant regardless of query param
	ctx := WithPrincipal(context.Background(), &Principal{Scope: ScopeMerchant, MerchantID: "m1"})
	if got := AllowedMerchant(ctx, "m_other"); got != "m1" {
		t.Fatalf("merchant scope should force m1, got %q", got)
	}
	// admin: query param wins
	ctx = WithPrincipal(context.Background(), &Principal{Scope: ScopeAdmin})
	if got := AllowedMerchant(ctx, "m_query"); got != "m_query" {
		t.Fatalf("admin scope should pass query, got %q", got)
	}
}

func TestParseKeyPrefix(t *testing.T) {
	cases := map[string][2]string{
		"rsk_live_merchant1_xyz":  {"live", "merchant1"},
		"rsk_test_merchant1_xyz":  {"test", "merchant1"},
		"rsk_admin_xyz":           {"admin", "xyz"},
		"not-our-format":          {"", ""},
		"":                        {"", ""},
	}
	for in, want := range cases {
		env, prefix := ParseKeyPrefix(in)
		if env != want[0] || prefix != want[1] {
			t.Fatalf("ParseKeyPrefix(%q) = (%q, %q), want %v", in, env, prefix, want)
		}
	}
}

func TestHashKey_DeterministicAndOpaque(t *testing.T) {
	a := HashKey("rsk_live_m1_xyz")
	b := HashKey("rsk_live_m1_xyz")
	if a != b {
		t.Fatal("hash should be deterministic")
	}
	if len(a) != 64 {
		t.Fatalf("sha256 hex should be 64 chars, got %d", len(a))
	}
	if a == "rsk_live_m1_xyz" {
		t.Fatal("hash should not equal plaintext")
	}
}
