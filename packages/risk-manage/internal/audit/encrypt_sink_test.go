package audit

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// staticKeyProvider 单 key 测试用，绕过 env。
type staticKeyProvider struct {
	id  string
	key []byte
}

func (p *staticKeyProvider) ActiveKeyID() string             { return p.id }
func (p *staticKeyProvider) DataKey(id string) ([]byte, error) {
	if id != p.id {
		return nil, errKeyNotFound
	}
	return p.key, nil
}

var errKeyNotFound = makeErr("key not found")

type errStr string

func (e errStr) Error() string { return string(e) }

func makeErr(s string) error { return errStr(s) }

// captureSink 内存收集，验证 EncryptSink 的输出形态。
type captureSink struct {
	got []*DecisionAudit
}

func (c *captureSink) Write(_ context.Context, a *DecisionAudit) {
	cp := *a
	c.got = append(c.got, &cp)
}

func mustHexKey(t *testing.T) []byte {
	t.Helper()
	// 32 byte 全零 key → AES-256 合法但仅测试用
	b, _ := hex.DecodeString(strings.Repeat("0a", 32))
	return b
}

func TestEncryptSink_RoundTrip(t *testing.T) {
	keys := &staticKeyProvider{id: "v1", key: mustHexKey(t)}
	cap := &captureSink{}
	sink := NewEncryptSink(cap, keys, zap.NewNop())

	orig := &DecisionAudit{
		DecisionID: "abc123",
		OccurredAt: time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC),
		Verdict:    "REVIEW",
		RiskScore:  42,
		Input: AuditInput{
			MerchantID: "m1", CustomerID: "c1", Amount: 100,
			IPAddress: "1.2.3.4", DeviceID: "dev-x",
		},
		Hits: []AuditHit{{RuleID: "r1", RuleName: "limit", Decision: "REVIEW", Detail: "amount > 50"}},
	}
	sink.Write(context.Background(), orig)

	if len(cap.got) != 1 {
		t.Fatalf("expected 1 captured, got %d", len(cap.got))
	}
	enc := cap.got[0]
	if enc.Input.IPAddress != "" || enc.Input.DeviceID != "" || enc.Input.MerchantID != "" {
		t.Fatal("encrypted output must not carry plaintext PII in Input fields")
	}
	if enc.Input.Metadata["enc_ct_b64"] == "" {
		t.Fatal("ciphertext envelope missing")
	}
	// IncludeHitsPlaintext=true → rule_id 保留，detail 应清空
	if len(enc.Hits) != 1 || enc.Hits[0].RuleID != "r1" || enc.Hits[0].Detail != "" {
		t.Fatalf("hits scrub failed: %+v", enc.Hits)
	}

	dec, err := Decrypt(enc, keys)
	if err != nil {
		t.Fatal(err)
	}
	if dec.DecisionID != orig.DecisionID || dec.Input.IPAddress != "1.2.3.4" ||
		dec.Input.DeviceID != "dev-x" || dec.RiskScore != 42 {
		t.Fatalf("roundtrip mismatch: %+v", dec)
	}
	if len(dec.Hits) != 1 || dec.Hits[0].Detail != "amount > 50" {
		t.Fatalf("hits detail not preserved in ciphertext: %+v", dec.Hits)
	}
}

func TestEncryptSink_TamperedCiphertextRejected(t *testing.T) {
	keys := &staticKeyProvider{id: "v1", key: mustHexKey(t)}
	cap := &captureSink{}
	sink := NewEncryptSink(cap, keys, zap.NewNop())
	sink.Write(context.Background(), &DecisionAudit{DecisionID: "id1", Verdict: "ALLOW"})

	enc := cap.got[0]
	// 篡改一个 byte
	ct := enc.Input.Metadata["enc_ct_b64"]
	enc.Input.Metadata["enc_ct_b64"] = "AA" + ct[2:]
	if _, err := Decrypt(enc, keys); err == nil {
		t.Fatal("expected GCM auth failure on tampered ciphertext")
	}
}

func TestEncryptSink_DifferentDecisionIDFailsAAD(t *testing.T) {
	keys := &staticKeyProvider{id: "v1", key: mustHexKey(t)}
	cap := &captureSink{}
	sink := NewEncryptSink(cap, keys, zap.NewNop())
	sink.Write(context.Background(), &DecisionAudit{DecisionID: "id_a", Verdict: "ALLOW"})

	// 把 ciphertext 移到另一个 decision_id 的 envelope → AAD mismatch
	enc := cap.got[0]
	enc.DecisionID = "id_b"
	if _, err := Decrypt(enc, keys); err == nil {
		t.Fatal("expected AAD mismatch failure when decision_id changed")
	}
}

func TestEncryptSink_FailOpenOnMissingKey(t *testing.T) {
	keys := &staticKeyProvider{id: "v_missing", key: nil}
	cap := &captureSink{}
	sink := NewEncryptSink(cap, keys, zap.NewNop())
	sink.Write(context.Background(), &DecisionAudit{DecisionID: "id1"})
	if len(cap.got) != 0 {
		t.Fatal("missing key must drop record (fail-secure), not write plaintext")
	}
}
