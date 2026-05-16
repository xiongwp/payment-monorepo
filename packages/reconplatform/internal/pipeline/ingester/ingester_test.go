package ingester

import (
	"testing"
	"time"

	"reconcile-system/internal/cdc"
)

func TestCdcToStore_FieldsCopied(t *testing.T) {
	ts := time.Now()
	src := &cdc.Event{
		Service: "pc", Schema: "pc_db_5", Table: "tx", PK: "tx_1",
		Op: cdc.OpInsert,
		After:      map[string]any{"id": "tx_1"},
		BinlogFile: "mysql-bin.000123", BinlogPos: 4096,
		Timestamp: ts,
		Indexes:   map[string]string{"pi_id": "pi_abc"},
	}
	out := cdcToStore(src)
	if out.Service != "pc" || out.PK != "tx_1" || out.Op != "INSERT" {
		t.Errorf("basic fields wrong: %+v", out)
	}
	if out.Indexes["pi_id"] != "pi_abc" {
		t.Errorf("indexes lost")
	}
	if !out.Timestamp.Equal(ts) {
		t.Errorf("ts not preserved")
	}
}

func TestNumStr(t *testing.T) {
	cases := map[int]string{
		0: "0", 1: "1", 9: "9", 10: "10", 42: "42", 1234: "1234",
		-1: "-1", -42: "-42",
	}
	for in, want := range cases {
		if got := numStr(in); got != want {
			t.Errorf("numStr(%d) = %q want %q", in, got, want)
		}
	}
}

func TestDefaultConfig_Fields(t *testing.T) {
	c := DefaultConfig([]string{"kafka:9092"}, []string{"recon.cdc.pc"})
	if c.ConsumerGroup != "reconplatform-ingester" {
		t.Error("group default")
	}
	if c.MaxBatch != 500 {
		t.Error("batch default")
	}
	if c.DLQTopic == "" {
		t.Error("DLQ default missing")
	}
}
