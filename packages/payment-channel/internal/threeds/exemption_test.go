package threeds

import "testing"

func base() AuthRequest {
	return AuthRequest{
		AmountCents:       50_00, // €50
		Currency:          "EUR",
		CardScheme:        "visa",
		BIN:               "453204",
		MerchantID:        "m_1",
		IssuerCountry:     "DE", // EEA, 触发 PSD2
		RiskScore:         20,
		AcquirerFraudRate: 0.0005, // 0.05%, 低于 €100 段阈值
	}
}

func TestNonEEACard_SkipsThreeDS(t *testing.T) {
	r := base()
	r.IssuerCountry = "US"
	d := Decide(r)
	if d.Action != ActionNonThreeDS {
		t.Errorf("non-EEA action = %s, want non_3ds", d.Action)
	}
	if d.LiabilityShift {
		t.Error("non-3DS should not shift liability")
	}
}

func TestTRA_PassesForLowAmountLowRisk(t *testing.T) {
	r := base()
	r.AmountCents = 80_00 // €80, in €100 segment
	r.AcquirerFraudRate = 0.0010 // 0.10%, below 0.13% threshold
	d := Decide(r)
	if d.Action != ActionFrictionless {
		t.Errorf("action = %s, want frictionless", d.Action)
	}
	if d.ExemptionUsed != ExemptionTRA {
		t.Errorf("exemption = %s, want tra", d.ExemptionUsed)
	}
	if d.ECI != "06" {
		t.Errorf("visa frictionless eci = %s, want 06", d.ECI)
	}
}

func TestTRA_RejectsHighFraudRate(t *testing.T) {
	r := base()
	r.AmountCents = 80_00
	r.AcquirerFraudRate = 0.0020 // 0.20% — 超 €100 段 0.13%
	d := Decide(r)
	if d.Action != ActionChallenge {
		t.Errorf("high fraud rate should challenge, got %s", d.Action)
	}
}

func TestTRA_RejectsHighAmount(t *testing.T) {
	r := base()
	r.AmountCents = 600_00 // €600, > €500 max TRA segment
	d := Decide(r)
	if d.Action != ActionChallenge {
		t.Errorf("over €500 should challenge, got %s", d.Action)
	}
}

func TestLowValue_SingleTxnUnder30(t *testing.T) {
	r := base()
	r.AmountCents = 20_00
	r.CardLowValueCount = 2
	r.CardLowValueAmount = 50_00
	d := Decide(r)
	if d.ExemptionUsed != ExemptionLowValue {
		t.Errorf("expected low_value, got %s", d.ExemptionUsed)
	}
}

func TestLowValue_RollingCountExceeded(t *testing.T) {
	r := base()
	r.AmountCents = 20_00
	r.CardLowValueCount = 5 // 已用满
	d := Decide(r)
	if d.ExemptionUsed == ExemptionLowValue {
		t.Error("should not use low_value when counter at max")
	}
}

func TestMIT_FirstRequiresChallenge(t *testing.T) {
	r := base()
	r.AmountCents = 5000_00 // 大额, 否则 TRA 命中
	r.IsRecurring = true
	r.IsFirstMIT = true
	d := Decide(r)
	if d.Action != ActionChallenge {
		t.Errorf("first MIT must SCA, got %s", d.Action)
	}
}

func TestMIT_SubsequentFrictionless(t *testing.T) {
	r := base()
	r.AmountCents = 5000_00
	r.IsRecurring = true
	r.IsFirstMIT = false
	d := Decide(r)
	if d.ExemptionUsed != ExemptionMITRecurring {
		t.Errorf("subsequent MIT should use exemption, got %s", d.ExemptionUsed)
	}
}

func TestTrusted_AlwaysFrictionless(t *testing.T) {
	r := base()
	r.AmountCents = 99999_00
	r.IsTrusted = true
	d := Decide(r)
	if d.ExemptionUsed != ExemptionTrusted {
		t.Errorf("trusted should override, got %s", d.ExemptionUsed)
	}
	if !d.LiabilityShift {
		t.Error("trusted should still have liability shift")
	}
}

func TestMastercard_DifferentECI(t *testing.T) {
	r := base()
	r.CardScheme = "mastercard"
	r.AmountCents = 80_00
	d := Decide(r)
	if d.ECI != "01" {
		t.Errorf("mastercard frictionless eci = %s, want 01", d.ECI)
	}
}
