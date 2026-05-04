package store

import (
	"context"
	"testing"
	"time"
)

func TestWarmup_RebuildsLinks(t *testing.T) {
	links := NewMemLinkStore()
	rows := []AuditRow{
		{
			OccurredAt: time.Now(),
			MerchantID: "m1", CustomerID: "u100", IPAddress: "1.1.1.1", DeviceID: "d1",
			Metadata: map[string]string{"fingerprint_hash": "fp1"},
		},
		{
			OccurredAt: time.Now(),
			MerchantID: "m1", CustomerID: "u101", IPAddress: "1.1.1.1", DeviceID: "d2",
			Metadata: map[string]string{"fingerprint_hash": "fp1"},
		},
	}
	edges := Warmup(context.Background(), links, rows)
	if edges == 0 {
		t.Fatal("warmup should write edges")
	}
	// 同 IP 应该关联到 2 个 customer
	custs := links.Peers(context.Background(), "ip:1.1.1.1", "customer:")
	if len(custs) != 2 {
		t.Errorf("ip:1.1.1.1 should link to 2 customers; got %d", len(custs))
	}
	// 同 fp 也应该关联到 2 个 customer
	fpCusts := links.Peers(context.Background(), "fp:fp1", "customer:")
	if len(fpCusts) != 2 {
		t.Errorf("fp:fp1 should link to 2 customers; got %d", len(fpCusts))
	}
}

func TestWarmup_NilLinksOrEmptyRowsSafe(t *testing.T) {
	if got := Warmup(context.Background(), nil, []AuditRow{{}}); got != 0 {
		t.Error("nil links should return 0")
	}
	if got := Warmup(context.Background(), NewMemLinkStore(), nil); got != 0 {
		t.Error("nil rows should return 0")
	}
}

func TestWarmup_SkipsEmptyPivots(t *testing.T) {
	links := NewMemLinkStore()
	// 全空 row 应该不写边
	rows := []AuditRow{{OccurredAt: time.Now()}}
	if got := Warmup(context.Background(), links, rows); got != 0 {
		t.Errorf("empty row should yield 0 edges, got %d", got)
	}
}
