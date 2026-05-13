package candidate

import (
	"context"
	"testing"
	"time"

	"reconcile-system/internal/store"
)

func evt(svc, table, pk string, indexes map[string]string) *store.Event {
	return &store.Event{
		Service: svc, Table: table, PK: pk,
		Op: "insert", Timestamp: time.Now(),
		Indexes: indexes,
	}
}

func TestMemory_PutBelowThreshold_NoTrigger(t *testing.T) {
	m := NewMemoryLayer(Config{DefaultTriggerThreshold: 3})
	got, err := m.Put(context.Background(),
		evt("pc", "tx", "tx_1", map[string]string{"pi_id": "pi_1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expect 0 trigger under threshold, got %d", len(got))
	}
}

func TestMemory_PutHitsThreshold_Trigger(t *testing.T) {
	m := NewMemoryLayer(Config{DefaultTriggerThreshold: 2})
	_, _ = m.Put(context.Background(), evt("pc", "tx", "tx_1", map[string]string{"pi_id": "pi_1"}))
	got, err := m.Put(context.Background(), evt("oc", "pi", "pi_1", map[string]string{"pi_id": "pi_1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expect 1 trigger at threshold, got %d", len(got))
	}
	if got[0].BizKey != "pi_id" || got[0].Value != "pi_1" {
		t.Errorf("trigger wrong: %+v", got[0])
	}
}

func TestMemory_Get_ReturnsAllEventsInBucket(t *testing.T) {
	m := NewMemoryLayer(Config{DefaultTriggerThreshold: 5})
	_, _ = m.Put(context.Background(), evt("pc", "tx", "tx_1", map[string]string{"pi_id": "pi_1"}))
	_, _ = m.Put(context.Background(), evt("oc", "pi", "pi_1", map[string]string{"pi_id": "pi_1"}))
	_, _ = m.Put(context.Background(), evt("ac", "le", "le_1", map[string]string{"pi_id": "pi_1"}))
	got, _ := m.Get(context.Background(), "pi_id", "pi_1")
	if len(got) != 3 {
		t.Errorf("want 3 events, got %d", len(got))
	}
}

func TestMemory_PopAndAck(t *testing.T) {
	m := NewMemoryLayer(Config{DefaultTriggerThreshold: 1})
	_, _ = m.Put(context.Background(), evt("pc", "tx", "tx_1", map[string]string{"pi_id": "pi_a"}))
	_, _ = m.Put(context.Background(), evt("pc", "tx", "tx_2", map[string]string{"pi_id": "pi_b"}))

	triggers, _ := m.Pop(context.Background(), 10, 0)
	if len(triggers) != 2 {
		t.Fatalf("want 2 triggers, got %d", len(triggers))
	}

	// Ack one
	_ = m.AckMatch(context.Background(), triggers[0], false)
	got, _ := m.Get(context.Background(), triggers[0].BizKey, triggers[0].Value)
	if len(got) != 0 {
		t.Errorf("after Ack bucket should be empty, got %d", len(got))
	}
}

func TestMemory_LockExclusion(t *testing.T) {
	m := NewMemoryLayer(Config{})
	t1 := TriggerKey{BizKey: "pi_id", Value: "pi_x"}
	unlock, ok, _ := m.Lock(context.Background(), t1)
	if !ok {
		t.Fatal("first lock should succeed")
	}
	_, ok2, _ := m.Lock(context.Background(), t1)
	if ok2 {
		t.Error("second lock on same key should fail")
	}
	unlock()
	_, ok3, _ := m.Lock(context.Background(), t1)
	if !ok3 {
		t.Error("after unlock should re-acquire")
	}
}

func TestMemory_EmptyIndexSkipped(t *testing.T) {
	m := NewMemoryLayer(Config{DefaultTriggerThreshold: 1})
	got, err := m.Put(context.Background(), evt("pc", "tx", "tx_1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Error("event with no indexes should not trigger")
	}
}

func TestMemory_PerBizKeyThreshold(t *testing.T) {
	m := NewMemoryLayer(Config{
		DefaultTriggerThreshold: 10, // very high default
		TriggerThreshold: map[string]int{
			"pi_id": 2, // override for pi_id
		},
	})
	_, _ = m.Put(context.Background(), evt("pc", "tx", "tx_1", map[string]string{"pi_id": "pi_a"}))
	got, _ := m.Put(context.Background(), evt("oc", "pi", "pi_a", map[string]string{"pi_id": "pi_a"}))
	if len(got) != 1 {
		t.Error("per-biz threshold should fire at 2")
	}
}

func TestMemory_TriggerKeyParse(t *testing.T) {
	t1, err := ParseTrigger("pi_id:pi_abc")
	if err != nil || t1.BizKey != "pi_id" || t1.Value != "pi_abc" {
		t.Errorf("parse failed: %+v err=%v", t1, err)
	}
	if _, err := ParseTrigger("nocolon"); err == nil {
		t.Error("expect error for no-colon")
	}
}

func TestMemory_StatsCounts(t *testing.T) {
	m := NewMemoryLayer(Config{DefaultTriggerThreshold: 5})
	_, _ = m.Put(context.Background(), evt("pc", "tx", "tx_1", map[string]string{"pi_id": "pi_a"}))
	_, _ = m.Put(context.Background(), evt("pc", "tx", "tx_2", map[string]string{"pi_id": "pi_b"}))
	_, _ = m.Put(context.Background(), evt("pc", "tx", "tx_3", map[string]string{"idempotency_key": "i_x"}))
	s, _ := m.Stats(context.Background())
	if s["bucket_pi_id"] != 2 {
		t.Errorf("pi_id buckets wrong: %d", s["bucket_pi_id"])
	}
	if s["bucket_idempotency_key"] != 1 {
		t.Errorf("idem bucket wrong: %d", s["bucket_idempotency_key"])
	}
}
