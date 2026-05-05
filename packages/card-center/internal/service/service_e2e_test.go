// E2E test for card-center service layer:
//
//	Tokenize → CreatePaymentToken → Detokenize → 二次 Detokenize 拒（一次性）→
//	错 user 拒 → 错 pi 拒 → 删卡 → 续操作仍能解（KMS rotation 才物理失效）
//
// 用 fakeKMS（vault 单测里那个）+ in-memory repos + nil audit emitter 跑通整条链路。
// 不依赖真实 gRPC / Kafka / DB。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/card-center/internal/repo"
	"github.com/xiongwp/card-center/internal/vault"
)

func TestE2E_StoreCard_CreatePaymentToken_Detokenize_OneTime(t *testing.T) {
	svc, _ := newE2EService(t)
	ctx := context.Background()

	// ── 1. 用户存卡 ─────────────────────────────────────────────────
	tokOut, err := svc.Tokenize(ctx, &TokenizeInput{
		UserID:     1001,
		PAN:        "4111111111111111",
		ExpMonth:   12,
		ExpYear:    2030,
		HolderName: "Alice",
		Caller:     "frontend-sdk",
		CallerIP:   "10.0.0.5",
		TraceID:    "trace_001",
	})
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if tokOut.MaskedPAN != "411111******1111" {
		t.Fatalf("masked: %s", tokOut.MaskedPAN)
	}
	if tokOut.Network != "visa" {
		t.Fatalf("network: %s", tokOut.Network)
	}
	storedToken := tokOut.StoredToken
	t.Logf("stored_token: %s (len=%d)", storedToken[:50]+"...", len(storedToken))

	// ── 2. order-core 拿 stored_token 派生支付 token ──────────────────
	payOut, err := svc.CreatePaymentToken(ctx, &CreatePaymentTokenInput{
		StoredToken: storedToken,
		UserID:      1001,
		PIID:        "pi_5511_001",
		Amount:      10000,
		Currency:    "PHP",
		TTL:         30 * time.Minute,
		Caller:      "order-core",
		CallerIP:    "10.0.1.5",
		TraceID:     "trace_002",
	})
	if err != nil {
		t.Fatalf("CreatePaymentToken: %v", err)
	}
	if payOut.MaskedPAN != "411111******1111" {
		t.Fatalf("payment masked: %s", payOut.MaskedPAN)
	}
	if payOut.ExpiresAt.Before(time.Now().Add(29 * time.Minute)) {
		t.Fatalf("TTL too short: %v", payOut.ExpiresAt)
	}
	paymentToken := payOut.PaymentToken
	t.Logf("payment_token TTL: %v", time.Until(payOut.ExpiresAt))

	// ── 3. card-payment 调 Detokenize 拿 PAN ─────────────────────────
	detok, err := svc.Detokenize(ctx, &DetokenizeInput{
		PaymentToken: paymentToken,
		PIID:         "pi_5511_001",
		Caller:       "card-payment",
		CallerIP:     "10.0.2.5",
		TraceID:      "trace_003",
	})
	if err != nil {
		t.Fatalf("Detokenize: %v", err)
	}
	if detok.PAN != "4111111111111111" {
		t.Fatalf("PAN mismatch: %s", detok.PAN)
	}
	if detok.MaskedPAN != "411111******1111" {
		t.Fatalf("masked: %s", detok.MaskedPAN)
	}

	// ── 4. 重放：同 token 第二次 Detokenize → 拒（一次性 enforcement） ──
	_, err = svc.Detokenize(ctx, &DetokenizeInput{
		PaymentToken: paymentToken,
		PIID:         "pi_5511_001",
		Caller:       "card-payment",
		TraceID:      "trace_004_replay",
	})
	if err == nil {
		t.Fatal("replay should be rejected; got nil")
	}
	t.Logf("replay correctly rejected: %v", err)
}

func TestE2E_PaymentToken_WrongPIID_Rejected(t *testing.T) {
	svc, _ := newE2EService(t)
	ctx := context.Background()

	tokOut, _ := svc.Tokenize(ctx, &TokenizeInput{
		UserID: 1002, PAN: "5234567890123456", ExpMonth: 6, ExpYear: 2030, Caller: "frontend-sdk",
	})
	payOut, _ := svc.CreatePaymentToken(ctx, &CreatePaymentTokenInput{
		StoredToken: tokOut.StoredToken, UserID: 1002,
		PIID: "pi_legit_001", Amount: 5000, Currency: "PHP",
		TTL: 30 * time.Minute, Caller: "order-core",
	})

	// 攻击者拿 payment_token 试图用到别的 PI
	_, err := svc.Detokenize(ctx, &DetokenizeInput{
		PaymentToken: payOut.PaymentToken,
		PIID:         "pi_evil_999", // 错的 pi_id
		Caller:       "card-payment",
	})
	if err == nil {
		t.Fatal("wrong pi_id should be rejected (AAD mismatch)")
	}
	t.Logf("wrong pi_id correctly rejected: %v", err)
}

func TestE2E_StoredToken_WrongUser_Rejected(t *testing.T) {
	svc, _ := newE2EService(t)
	ctx := context.Background()

	tokOut, _ := svc.Tokenize(ctx, &TokenizeInput{
		UserID: 2001, PAN: "4111222233334444", ExpMonth: 12, ExpYear: 2030, Caller: "frontend-sdk",
	})

	// 攻击者拿 stored_token 试图用到别人 user
	_, err := svc.CreatePaymentToken(ctx, &CreatePaymentTokenInput{
		StoredToken: tokOut.StoredToken,
		UserID:      9999, // 错的 user_id
		PIID:        "pi_x",
		Amount:      100, Currency: "PHP",
		Caller: "order-core",
	})
	if err == nil {
		t.Fatal("wrong user_id should be rejected (AAD mismatch + DB lookup miss)")
	}
	t.Logf("wrong user_id correctly rejected: %v", err)
}

func TestE2E_DeleteCard_PreventsCreatePaymentToken(t *testing.T) {
	svc, _ := newE2EService(t)
	ctx := context.Background()

	tokOut, _ := svc.Tokenize(ctx, &TokenizeInput{
		UserID: 3001, PAN: "4111111111111111", ExpMonth: 12, ExpYear: 2030, Caller: "frontend-sdk",
	})

	// 用户删卡
	if err := svc.DeleteCard(ctx, 3001, tokOut.StoredToken, "user request",
		"user-merchant-core", "10.0.0.5", "trace_del"); err != nil {
		t.Fatalf("DeleteCard: %v", err)
	}

	// 删后 CreatePaymentToken 拒（DB 查 stored_token row 是 status=deleted）
	_, err := svc.CreatePaymentToken(ctx, &CreatePaymentTokenInput{
		StoredToken: tokOut.StoredToken, UserID: 3001,
		PIID: "pi_after_delete", Amount: 100, Currency: "PHP",
		Caller: "order-core",
	})
	if err == nil {
		t.Fatal("deleted card should reject CreatePaymentToken")
	}
	t.Logf("deleted card correctly rejected: %v", err)
}

func TestE2E_PaymentToken_TTLExpired_Rejected(t *testing.T) {
	clock := &clockStub{t: time.Now()}
	svc, _ := newE2EServiceWithClock(t, clock.Now)
	ctx := context.Background()

	tokOut, _ := svc.Tokenize(ctx, &TokenizeInput{
		UserID: 4001, PAN: "4111111111111111", ExpMonth: 12, ExpYear: 2030, Caller: "frontend-sdk",
	})
	payOut, _ := svc.CreatePaymentToken(ctx, &CreatePaymentTokenInput{
		StoredToken: tokOut.StoredToken, UserID: 4001,
		PIID: "pi_ttl_001", Amount: 100, Currency: "PHP",
		TTL:    5 * time.Second, // 短 TTL
		Caller: "order-core",
	})

	// 推进时钟超过 TTL
	clock.t = clock.t.Add(10 * time.Second)

	_, err := svc.Detokenize(ctx, &DetokenizeInput{
		PaymentToken: payOut.PaymentToken,
		PIID:         "pi_ttl_001",
		Caller:       "card-payment",
	})
	if !errors.Is(err, vault.ErrTokenExpired) {
		// MarkUsed 先发生（成功 INSERT），然后 vault.Decrypt 走 TTL 校验返 ErrTokenExpired
		// 看下游逻辑包装可能 wrap 一下 — 用 strings 兜底也接受
		if err == nil || !contains(err.Error(), "expired") {
			t.Fatalf("expired token should be rejected with ErrTokenExpired; got %v", err)
		}
	}
	t.Logf("expired token correctly rejected: %v", err)
}

func TestE2E_DetokenizeCallerWhitelist(t *testing.T) {
	svc, _ := newE2EService(t)
	ctx := context.Background()

	tokOut, _ := svc.Tokenize(ctx, &TokenizeInput{
		UserID: 5001, PAN: "4111111111111111", ExpMonth: 12, ExpYear: 2030, Caller: "frontend-sdk",
	})
	payOut, _ := svc.CreatePaymentToken(ctx, &CreatePaymentTokenInput{
		StoredToken: tokOut.StoredToken, UserID: 5001, PIID: "pi_caller_test",
		Amount: 100, Currency: "PHP", Caller: "order-core",
	})

	// 非 card-payment 调 Detokenize → 应被 service 层拒
	_, err := svc.Detokenize(ctx, &DetokenizeInput{
		PaymentToken: payOut.PaymentToken,
		PIID:         "pi_caller_test",
		Caller:       "evil-service", // 不在白名单
	})
	if err == nil {
		t.Fatal("non-card-payment caller should be rejected")
	}
	t.Logf("non-whitelisted caller correctly rejected: %v", err)
}

// ─── helpers ────────────────────────────────────────────────────────────────

func newE2EService(t *testing.T) (*Service, *recordingAudit) {
	return newE2EServiceWithClock(t, time.Now)
}

func newE2EServiceWithClock(t *testing.T, now func() time.Time) (*Service, *recordingAudit) {
	t.Helper()
	v := vault.NewVault(&fakeKMSE2E{}, now)
	stored := newMemStoredCardRepo()
	pay := newMemPaymentTokenRepo()
	audit := &recordingAudit{}
	return NewService(v, stored, pay, audit, zap.NewNop()), audit
}

// fakeKMSE2E echoes (aad, plaintext) as ciphertext, validates aad on decrypt.
type fakeKMSE2E struct{}

func (fakeKMSE2E) Encrypt(_ context.Context, plaintext []byte, aad string) (string, string, error) {
	wrap := struct {
		AAD string `json:"aad"`
		PT  []byte `json:"pt"`
	}{aad, plaintext}
	b, _ := json.Marshal(&wrap)
	return string(b), "v1", nil
}

func (fakeKMSE2E) Decrypt(_ context.Context, ct string, aad string) ([]byte, string, error) {
	var wrap struct {
		AAD string `json:"aad"`
		PT  []byte `json:"pt"`
	}
	if err := json.Unmarshal([]byte(ct), &wrap); err != nil {
		return nil, "", err
	}
	if wrap.AAD != aad {
		return nil, "", errors.New("aad mismatch")
	}
	return wrap.PT, "v1", nil
}

// memStoredCardRepo in-memory 实现 repo.StoredCardRepo
type memStoredCardRepo struct {
	mu   sync.Mutex
	rows map[string]*repo.StoredCardRow // tokenHash → row
}

func newMemStoredCardRepo() *memStoredCardRepo {
	return &memStoredCardRepo{rows: make(map[string]*repo.StoredCardRow)}
}

func (m *memStoredCardRepo) Insert(_ context.Context, row *repo.StoredCardRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[row.TokenHash]; ok {
		return errors.New("dup token_hash")
	}
	row.ID = int64(len(m.rows) + 1)
	m.rows[row.TokenHash] = row
	return nil
}

func (m *memStoredCardRepo) GetByTokenHash(_ context.Context, userID int64, tokenHash string) (*repo.StoredCardRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[tokenHash]
	if !ok || row.UserID != userID {
		return nil, repo.ErrStoredCardNotFound
	}
	return row, nil
}

func (m *memStoredCardRepo) ListActiveByUser(_ context.Context, userID int64) ([]*repo.StoredCardRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*repo.StoredCardRow
	for _, r := range m.rows {
		if r.UserID == userID && r.Status == "active" {
			cp := *r
			cp.StoredToken = ""
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memStoredCardRepo) SoftDelete(_ context.Context, userID int64, tokenHash, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[tokenHash]
	if !ok || row.UserID != userID {
		return repo.ErrStoredCardNotFound
	}
	row.Status = "deleted"
	now := time.Now()
	row.DeletedAt = &now
	return nil
}

// memPaymentTokenRepo in-memory 实现 repo.PaymentTokenRepo
type memPaymentTokenRepo struct {
	mu   sync.Mutex
	used map[string]*repo.PaymentTokenUsedRow // token_hash → row
}

func newMemPaymentTokenRepo() *memPaymentTokenRepo {
	return &memPaymentTokenRepo{used: make(map[string]*repo.PaymentTokenUsedRow)}
}

func (m *memPaymentTokenRepo) MarkUsed(_ context.Context, row *repo.PaymentTokenUsedRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.used[row.TokenHash]; ok {
		return repo.ErrPaymentTokenAlreadyUsed
	}
	row.ID = int64(len(m.used) + 1)
	m.used[row.TokenHash] = row
	return nil
}

func (m *memPaymentTokenRepo) PurgeExpired(_ context.Context, cutoff time.Time, _ int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for k, r := range m.used {
		if r.ExpiresAt.Before(cutoff) {
			delete(m.used, k)
			n++
		}
	}
	return n, nil
}

// recordingAudit 收所有 audit 事件方便断言
type recordingAudit struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (r *recordingAudit) Emit(_ context.Context, ev AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

type clockStub struct{ t time.Time }

func (c *clockStub) Now() time.Time { return c.t }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
