package ipintel

import (
	"context"
	"testing"
)

func TestMemService_ExactMatch(t *testing.T) {
	m := NewMemService()
	m.Set("1.2.3.4", Result{VPN: true, Country: "US"})
	got := m.Lookup(context.Background(), "1.2.3.4")
	if !got.VPN || got.Country != "US" {
		t.Fatalf("exact match failed: %+v", got)
	}
}

func TestMemService_CIDRMatch(t *testing.T) {
	m := NewMemService()
	m.Set("10.0.0.0/8", Result{DataCenter: true})
	got := m.Lookup(context.Background(), "10.5.6.7")
	if !got.DataCenter {
		t.Fatalf("CIDR match failed: %+v", got)
	}
}

func TestMemService_LongestPrefixWins(t *testing.T) {
	m := NewMemService()
	m.Set("10.0.0.0/8", Result{DataCenter: true, Country: "US"})
	m.Set("10.0.0.0/16", Result{DataCenter: true, Country: "JP", VPN: true})
	got := m.Lookup(context.Background(), "10.0.5.5")
	if got.Country != "JP" {
		t.Fatalf("expected JP from /16; got %+v", got)
	}
	if !got.VPN {
		t.Fatalf("expected VPN flag from /16; got %+v", got)
	}
}

func TestMemService_UnknownIP_ZeroValue(t *testing.T) {
	m := NewMemService()
	got := m.Lookup(context.Background(), "8.8.8.8")
	if got.Country != "" || got.VPN || got.DataCenter {
		t.Fatalf("unknown IP should return zero, got %+v", got)
	}
}

func TestMemService_EmptyIP(t *testing.T) {
	m := NewMemService()
	got := m.Lookup(context.Background(), "")
	if (got != Result{}) {
		t.Fatalf("empty ip should be zero, got %+v", got)
	}
}

func TestMemService_BadIPNoMatch(t *testing.T) {
	m := NewMemService()
	m.Set("10.0.0.0/8", Result{DataCenter: true})
	got := m.Lookup(context.Background(), "not-an-ip")
	if got.DataCenter {
		t.Fatal("invalid ip should not match CIDR")
	}
}
