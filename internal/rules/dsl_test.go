package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

func newDSL(t *testing.T, body string) engine.Rule {
	t.Helper()
	r, err := DSLFactory()("d1", "dsl", true, json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDSL_AmountGt(t *testing.T) {
	r := newDSL(t, `{"conditions":[{"field":"amount","op":"gt","value":1000}],"weight":15}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{Amount: 5000}); h == nil {
		t.Fatal("amount 5000 > 1000 should hit")
	}
	if h := r.Evaluate(context.Background(), &engine.TxnContext{Amount: 500}); h != nil {
		t.Fatalf("amount 500 should not hit, got %+v", h)
	}
}

func TestDSL_AND(t *testing.T) {
	r := newDSL(t, `{"conditions":[
		{"field":"amount","op":"gt","value":1000},
		{"field":"country","op":"in","values":["CN","HK"]}
	],"weight":20}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{Amount: 2000, Country: "CN"}); h == nil {
		t.Fatal("AND: both met should hit")
	}
	if h := r.Evaluate(context.Background(), &engine.TxnContext{Amount: 2000, Country: "US"}); h != nil {
		t.Fatalf("AND: country fail should miss, got %+v", h)
	}
}

func TestDSL_OR(t *testing.T) {
	r := newDSL(t, `{"match_any":true,"conditions":[
		{"field":"ip_proxy","op":"eq","value":true},
		{"field":"ip_vpn","op":"eq","value":true}
	]}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{IPProxy: true}); h == nil {
		t.Fatal("OR: any one should hit")
	}
	if h := r.Evaluate(context.Background(), &engine.TxnContext{}); h != nil {
		t.Fatal("OR: none → no hit")
	}
}

func TestDSL_CrossField(t *testing.T) {
	r := newDSL(t, `{"conditions":[
		{"field":"ip_country","op":"ne_field","other":"country"}
	]}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{IPCountry: "RU", Country: "US"}); h == nil {
		t.Fatal("ip_country != country should hit")
	}
	if h := r.Evaluate(context.Background(), &engine.TxnContext{IPCountry: "US", Country: "US"}); h != nil {
		t.Fatal("matching countries should not hit")
	}
}

func TestDSL_MetadataAccess(t *testing.T) {
	r := newDSL(t, `{"conditions":[
		{"field":"metadata.email_hash","op":"prefix","value":"vip_"}
	]}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"email_hash": "vip_abc"},
	}); h == nil {
		t.Fatal("metadata prefix match should hit")
	}
}

func TestDSL_PresentAbsent(t *testing.T) {
	r := newDSL(t, `{"conditions":[{"field":"device_id","op":"absent"}]}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{}); h == nil {
		t.Fatal("missing device_id should match 'absent'")
	}
	if h := r.Evaluate(context.Background(), &engine.TxnContext{DeviceID: "x"}); h != nil {
		t.Fatal("present device_id should not match 'absent'")
	}
}

func TestDSL_DenyDecision(t *testing.T) {
	r := newDSL(t, `{"decision":"deny","conditions":[{"field":"amount","op":"gte","value":99999999}]}`)
	h := r.Evaluate(context.Background(), &engine.TxnContext{Amount: 100000000})
	if h == nil || h.Decision != engine.Deny {
		t.Fatalf("expected DENY, got %+v", h)
	}
}

func TestDSL_InvalidOpRejected(t *testing.T) {
	_, err := DSLFactory()("d1", "x", true, json.RawMessage(`{"conditions":[{"field":"amount","op":"weird","value":1}]}`))
	if err == nil {
		t.Fatal("invalid op should fail factory")
	}
}

func TestDSL_NotIn(t *testing.T) {
	r := newDSL(t, `{"conditions":[{"field":"payment_method","op":"not_in","values":["CARD"]}]}`)
	if h := r.Evaluate(context.Background(), &engine.TxnContext{PaymentMethod: "GCASH"}); h == nil {
		t.Fatal("GCASH not in [CARD] should hit")
	}
	if h := r.Evaluate(context.Background(), &engine.TxnContext{PaymentMethod: "CARD"}); h != nil {
		t.Fatal("CARD in [CARD] → not_in false → not hit")
	}
}
