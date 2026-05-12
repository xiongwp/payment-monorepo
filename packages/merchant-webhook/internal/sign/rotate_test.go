package sign

import (
	"strings"
	"testing"
	"time"
)

func TestComputeMulti_HasBothSigs(t *testing.T) {
	hdr := ComputeMulti([]string{"sec_old", "sec_new"}, time.Now().Unix(), []byte(`{"x":1}`))
	if !strings.Contains(hdr, "v1=") {
		t.Fatal("missing v1=")
	}
	if strings.Count(hdr, "v1=") != 2 {
		t.Errorf("want 2 v1 sigs, got header: %s", hdr)
	}
	if !strings.Contains(hdr, "t=") {
		t.Error("missing t=")
	}
}

func TestVerifyMulti_AcceptsAnySecret(t *testing.T) {
	ts := time.Now().Unix()
	body := []byte(`{"event":"refund.created"}`)
	hdr := ComputeMulti([]string{"sec_old", "sec_new"}, ts, body)

	// 用旧 secret 校验
	if err := VerifyMulti([]string{"sec_old"}, hdr, body, 60); err != nil {
		t.Errorf("verify with old secret failed: %v", err)
	}
	// 用新 secret 校验
	if err := VerifyMulti([]string{"sec_new"}, hdr, body, 60); err != nil {
		t.Errorf("verify with new secret failed: %v", err)
	}
	// 两个都给
	if err := VerifyMulti([]string{"sec_old", "sec_new"}, hdr, body, 60); err != nil {
		t.Errorf("verify with both secrets failed: %v", err)
	}
}

func TestVerifyMulti_RejectsWrongSecret(t *testing.T) {
	ts := time.Now().Unix()
	body := []byte(`{"x":1}`)
	hdr := ComputeMulti([]string{"sec_a"}, ts, body)

	if err := VerifyMulti([]string{"sec_wrong"}, hdr, body, 60); err == nil {
		t.Error("expected verify failure with wrong secret")
	}
}

func TestVerifyMulti_RejectsExpiredTimestamp(t *testing.T) {
	old := time.Now().Add(-10 * time.Minute).Unix()
	body := []byte(`{"x":1}`)
	hdr := ComputeMulti([]string{"sec_a"}, old, body)
	if err := VerifyMulti([]string{"sec_a"}, hdr, body, 60); err == nil {
		t.Error("expected timestamp rejection")
	}
}

func TestComputeMulti_BackwardCompatibleWithSingleVerify(t *testing.T) {
	// 单 secret 头 — Compute 出的, Verify (单参) 应该能过
	ts := time.Now().Unix()
	body := []byte(`{"x":1}`)
	hdr := ComputeMulti([]string{"sec_a"}, ts, body)
	// 原 Verify 函数也应 work (header 只有 1 个 v1)
	if err := Verify("sec_a", hdr, body, 60); err != nil {
		t.Errorf("legacy Verify should accept single-v1 header: %v", err)
	}
}
