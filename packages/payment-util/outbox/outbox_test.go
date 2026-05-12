package outbox

import (
	"encoding/json"
	"testing"
)

func TestNewEvent_GeneratesValidEvent(t *testing.T) {
	type payload struct{ X int }
	e, err := NewEvent("order", "ord_1", "OrderCreated", payload{X: 42})
	if err != nil {
		t.Fatal(err)
	}
	if e.EventID == "" {
		t.Error("event_id empty")
	}
	if e.Status != StatusPending {
		t.Errorf("status = %s, want pending", e.Status)
	}
	var got payload
	if err := json.Unmarshal(e.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.X != 42 {
		t.Errorf("payload X = %d, want 42", got.X)
	}
	if err := e.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestEventValidate_MissingFields(t *testing.T) {
	cases := []Event{
		{},                                // 全空
		{EventID: "e1"},                   // type 空
		{EventID: "e1", EventType: "X"},   // payload 空
	}
	for i, e := range cases {
		if err := e.Validate(); err == nil {
			t.Errorf("case %d: expected validate error, got nil", i)
		}
	}
}
