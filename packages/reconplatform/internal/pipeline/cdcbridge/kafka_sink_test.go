package cdcbridge

import (
	"context"
	"testing"
	"time"

	"reconcile-system/internal/cdc"
)

func TestPartitionKey_BizIndexPriority(t *testing.T) {
	s := &KafkaSink{cfg: Config{
		PartitionByIndex: []string{"pi_id", "order_id"},
	}}
	e := &cdc.Event{
		Service: "pc", Table: "tx", PK: "x",
		Indexes: map[string]string{"pi_id": "pi_abc", "order_id": "ord_xyz"},
	}
	got := s.partitionKey(e)
	if got != "pi_id:pi_abc" {
		t.Errorf("priority wrong: got %q", got)
	}
}

func TestPartitionKey_FallbackSecondaryIndex(t *testing.T) {
	s := &KafkaSink{cfg: Config{
		PartitionByIndex: []string{"pi_id", "order_id"},
	}}
	e := &cdc.Event{
		Service: "pc", Table: "tx", PK: "x",
		Indexes: map[string]string{"order_id": "ord_xyz"},
	}
	got := s.partitionKey(e)
	if got != "order_id:ord_xyz" {
		t.Errorf("fallback wrong: %q", got)
	}
}

func TestPartitionKey_NoIndex_UsesSvcTablePK(t *testing.T) {
	s := &KafkaSink{cfg: Config{PartitionByIndex: []string{"pi_id"}}}
	e := &cdc.Event{Service: "pc", Table: "tx", PK: "x_42"}
	got := s.partitionKey(e)
	if got != "pc:tx:x_42" {
		t.Errorf("no-index fallback wrong: %q", got)
	}
}

func TestTopicFor(t *testing.T) {
	s := &KafkaSink{cfg: Config{TopicPrefix: "recon.cdc"}}
	if got := s.topicFor("order-core"); got != "recon.cdc.order-core" {
		t.Errorf("topic wrong: %q", got)
	}
}

func TestDefaultConfig_AppliedFields(t *testing.T) {
	c := DefaultConfig([]string{"kafka:9092"})
	if c.TopicPrefix != "recon.cdc" {
		t.Errorf("prefix default wrong")
	}
	if c.BatchTimeout != 5*time.Millisecond {
		t.Errorf("batch timeout wrong")
	}
	if len(c.PartitionByIndex) < 4 {
		t.Errorf("default indexes too few")
	}
}

// recordingSink 给 FanOutSink 测试用.
type recordingSink struct {
	calls int
	last  *cdc.Event
}

func (r *recordingSink) Publish(_ context.Context, e *cdc.Event) error {
	r.calls++
	r.last = e
	return nil
}

func TestFanOutSink_AllSinksCalled(t *testing.T) {
	s1 := &recordingSink{}
	s2 := &recordingSink{}
	fan := &FanOutSink{Sinks: []Sink{s1, s2}}
	e := &cdc.Event{Service: "x", Table: "y", PK: "z"}
	if err := fan.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if s1.calls != 1 || s2.calls != 1 {
		t.Errorf("not all sinks called: s1=%d s2=%d", s1.calls, s2.calls)
	}
}
