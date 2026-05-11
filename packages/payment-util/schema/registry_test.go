package schema

import (
	"context"
	"strings"
	"testing"
)

const v1 = `{"type":"object","required":["charge_id","amount_minor"],"properties":{
  "charge_id":{"type":"string"},"amount_minor":{"type":"integer"},"currency":{"type":"string"}
}}`

const v2AddOptional = `{"type":"object","required":["charge_id","amount_minor"],"properties":{
  "charge_id":{"type":"string"},"amount_minor":{"type":"integer"},"currency":{"type":"string"},
  "merchant_id":{"type":"string"}
}}`

const v2AddRequired = `{"type":"object","required":["charge_id","amount_minor","merchant_id"],"properties":{
  "charge_id":{"type":"string"},"amount_minor":{"type":"integer"},"merchant_id":{"type":"string"}
}}`

const v2TypeChange = `{"type":"object","required":["charge_id","amount_minor"],"properties":{
  "charge_id":{"type":"string"},"amount_minor":{"type":"string"}
}}`

func TestRegister_V1(t *testing.T) {
	r := NewMemoryRegistry()
	s, err := r.Register(context.Background(), "charge.succeeded", v1, Backward)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != 1 {
		t.Errorf("v=%d want 1", s.Version)
	}
}

func TestRegister_BackwardCompat_AddOptional(t *testing.T) {
	r := NewMemoryRegistry()
	_, _ = r.Register(context.Background(), "charge.succeeded", v1, Backward)
	_, err := r.Register(context.Background(), "charge.succeeded", v2AddOptional, Backward)
	if err != nil {
		t.Errorf("add optional field should be backward compat, got %v", err)
	}
}

func TestRegister_BackwardCompat_AddRequired_Fails(t *testing.T) {
	r := NewMemoryRegistry()
	_, _ = r.Register(context.Background(), "charge.succeeded", v1, Backward)
	_, err := r.Register(context.Background(), "charge.succeeded", v2AddRequired, Backward)
	if err == nil {
		t.Error("add required should break backward compat")
	}
}

func TestRegister_TypeChange_Fails(t *testing.T) {
	r := NewMemoryRegistry()
	_, _ = r.Register(context.Background(), "charge.succeeded", v1, Backward)
	_, err := r.Register(context.Background(), "charge.succeeded", v2TypeChange, Backward)
	if err == nil || !strings.Contains(err.Error(), "type changed") {
		t.Errorf("type change should fail: %v", err)
	}
}

func TestLatest(t *testing.T) {
	r := NewMemoryRegistry()
	_, _ = r.Register(context.Background(), "charge.succeeded", v1, Backward)
	_, _ = r.Register(context.Background(), "charge.succeeded", v2AddOptional, Backward)
	s, err := r.Latest(context.Background(), "charge.succeeded")
	if err != nil || s.Version != 2 {
		t.Errorf("latest=%v err=%v", s, err)
	}
}

func TestValidate_OK(t *testing.T) {
	r := NewMemoryRegistry()
	s, _ := r.Register(context.Background(), "charge.succeeded", v1, None)
	err := Validate(s, map[string]any{
		"charge_id": "ch_001", "amount_minor": 100,
	})
	if err != nil {
		t.Errorf("should validate: %v", err)
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	r := NewMemoryRegistry()
	s, _ := r.Register(context.Background(), "x", v1, None)
	err := Validate(s, map[string]any{"charge_id": "ch_001"})
	if err == nil {
		t.Error("missing required should fail")
	}
}

func TestValidate_WrongType(t *testing.T) {
	r := NewMemoryRegistry()
	s, _ := r.Register(context.Background(), "x", v1, None)
	err := Validate(s, map[string]any{
		"charge_id": "ch_001", "amount_minor": "not-a-number",
	})
	if err == nil {
		t.Error("wrong type should fail")
	}
}
