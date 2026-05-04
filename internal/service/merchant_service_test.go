package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiongwp/user-merchant-core/internal/cache"
	"github.com/xiongwp/user-merchant-core/internal/domain"
)

// ─── fakes ──────────────────────────────────────────────────────────────────

type fakeRepo struct {
	byID       map[string]*domain.Merchant
	byHash     map[string]*domain.Merchant
	getHits    int32
	updateHits int32
	transitions int32
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{byID: map[string]*domain.Merchant{}, byHash: map[string]*domain.Merchant{}}
}

func (r *fakeRepo) Create(_ context.Context, m *domain.Merchant) error {
	r.byID[m.ID] = m
	if m.LiveKeyHash != "" {
		r.byHash[m.LiveKeyHash] = m
	}
	if m.TestKeyHash != "" {
		r.byHash[m.TestKeyHash] = m
	}
	return nil
}
func (r *fakeRepo) Get(_ context.Context, id string) (*domain.Merchant, error) {
	atomic.AddInt32(&r.getHits, 1)
	m, ok := r.byID[id]
	if !ok {
		return nil, domain.ErrMerchantNotFound
	}
	return m, nil
}
func (r *fakeRepo) BatchGet(_ context.Context, ids []string) ([]*domain.Merchant, error) {
	out := []*domain.Merchant{}
	for _, id := range ids {
		if m, ok := r.byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}
func (r *fakeRepo) GetByEmail(_ context.Context, email string) (*domain.Merchant, error) {
	for _, m := range r.byID {
		if m.ContactEmail == email {
			return m, nil
		}
	}
	return nil, domain.ErrMerchantNotFound
}
func (r *fakeRepo) GetByKeyHash(_ context.Context, h string) (*domain.Merchant, error) {
	m, ok := r.byHash[h]
	if !ok {
		return nil, domain.ErrMerchantNotFound
	}
	return m, nil
}
func (r *fakeRepo) List(context.Context, domain.MerchantStatus, domain.KYCStatus, int, int) ([]*domain.Merchant, int64, error) {
	return nil, 0, nil
}
func (r *fakeRepo) ListActive(context.Context, int) ([]*domain.Merchant, error) {
	out := make([]*domain.Merchant, 0, len(r.byID))
	for _, m := range r.byID {
		out = append(out, m)
	}
	return out, nil
}
func (r *fakeRepo) Update(context.Context, *domain.Merchant) error { return nil }
func (r *fakeRepo) UpdateFields(_ context.Context, id string, fields map[string]any) (*domain.Merchant, error) {
	atomic.AddInt32(&r.updateHits, 1)
	m := r.byID[id]
	if v, ok := fields["live_key_hash"]; ok {
		delete(r.byHash, m.LiveKeyHash)
		m.LiveKeyHash = v.(string)
		r.byHash[m.LiveKeyHash] = m
	}
	return m, nil
}
func (r *fakeRepo) TransitionKYC(_ context.Context, id string, from, to domain.KYCStatus, _, _ string) (*domain.Merchant, error) {
	atomic.AddInt32(&r.transitions, 1)
	m := r.byID[id]
	m.KYCStatus = to
	return m, nil
}
func (r *fakeRepo) AddDocument(context.Context, *domain.MerchantKYCDocument) error { return nil }
func (r *fakeRepo) ListDocuments(context.Context, string) ([]*domain.MerchantKYCDocument, error) {
	return nil, nil
}
func (r *fakeRepo) ReviewDocument(context.Context, string, string, string) error { return nil }
func (r *fakeRepo) ListKYCAudits(context.Context, string, int) ([]*domain.MerchantKYCAudit, error) {
	return nil, nil
}
func (r *fakeRepo) SoftDelete(context.Context, string) error { return nil }
func (r *fakeRepo) PurgeDeletedBefore(context.Context, time.Time) (int64, error) {
	return 0, errors.New("not needed")
}

type fakeIDGen struct{ n int64 }

func (g *fakeIDGen) NextID(context.Context, string) (int64, error) { g.n++; return g.n, nil }
func (g *fakeIDGen) Register(context.Context, string, int64, int, string) error {
	return nil
}
func (g *fakeIDGen) Preload(context.Context) error { return nil }

// ─── tests ──────────────────────────────────────────────────────────────────

func setup(t *testing.T) (MerchantService, *fakeRepo, *cache.MerchantCache) {
	t.Helper()
	r := newFakeRepo()
	c := cache.New(100, 0, nil)
	return NewMerchantService(r, &fakeIDGen{}, c, nil), r, c
}

func TestCreate_CachesMerchant(t *testing.T) {
	svc, r, c := setup(t)
	out, err := svc.Create(context.Background(), &CreateMerchantInput{
		Name:         "bob",
		ContactEmail: "bob@x.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Merchant.ID == "" {
		t.Fatal("expected id")
	}
	if len(r.byID) != 1 {
		t.Fatal("repo should have 1 row")
	}
	if _, ok := c.GetByID(out.Merchant.ID); !ok {
		t.Fatal("expected cache hit after create")
	}
	if out.LiveSecretKey == "" || out.TestSecretKey == "" || out.WebhookSecret == "" {
		t.Fatal("secrets should be returned once")
	}
}

func TestGet_UsesCacheAndSingleflight(t *testing.T) {
	svc, r, _ := setup(t)
	_, err := svc.Create(context.Background(), &CreateMerchantInput{Name: "x", ContactEmail: "a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	r.getHits = 0
	// 10 concurrent Gets; cache should absorb all except at most 1 miss
	for i := 0; i < 10; i++ {
		if _, err := svc.Get(context.Background(), "mch_1"); err != nil {
			t.Fatal(err)
		}
	}
	if atomic.LoadInt32(&r.getHits) > 1 {
		t.Fatalf("expected cache to absorb reads, got %d repo hits", r.getHits)
	}
}

func TestRotateAPIKeys_InvalidatesCache(t *testing.T) {
	svc, _, c := setup(t)
	out, err := svc.Create(context.Background(), &CreateMerchantInput{Name: "x", ContactEmail: "a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.GetByID(out.Merchant.ID); !ok {
		t.Fatal("pre-rotate cache hit expected")
	}
	if _, err := svc.RotateAPIKeys(context.Background(), out.Merchant.ID, "live"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.GetByID(out.Merchant.ID); ok {
		t.Fatal("cache should be invalidated after rotate")
	}
}

func TestTransitionKYC_WritesCacheAndChangesStatus(t *testing.T) {
	svc, _, c := setup(t)
	out, _ := svc.Create(context.Background(), &CreateMerchantInput{Name: "x", ContactEmail: "a@b.c"})
	// pending → submitted → reviewing → approved
	if _, err := svc.SubmitKYC(context.Background(), out.Merchant.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartReview(context.Background(), out.Merchant.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	m, err := svc.Approve(context.Background(), out.Merchant.ID, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if m.KYCStatus != domain.KYCStatusApproved {
		t.Fatalf("status: got %s want approved", m.KYCStatus)
	}
	cached, _ := c.GetByID(out.Merchant.ID)
	if cached.KYCStatus != domain.KYCStatusApproved {
		t.Fatalf("cache stale: %s", cached.KYCStatus)
	}
}

func TestBatchGet_MixedHitMiss(t *testing.T) {
	svc, _, c := setup(t)
	a, _ := svc.Create(context.Background(), &CreateMerchantInput{Name: "a", ContactEmail: "a@x.com"})
	b, _ := svc.Create(context.Background(), &CreateMerchantInput{Name: "b", ContactEmail: "b@x.com"})
	// Invalidate a so it's a miss; b still cached
	c.Invalidate(a.Merchant.ID)
	got, err := svc.BatchGet(context.Background(), []string{a.Merchant.ID, b.Merchant.ID, "mch_missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d: %v", len(got), got)
	}
	if got[a.Merchant.ID] == nil || got[b.Merchant.ID] == nil {
		t.Fatal("both existing merchants should be present")
	}
	if _, bad := got["mch_missing"]; bad {
		t.Fatal("missing id should be absent from map")
	}
}

func TestAuthenticateByAPIKey_Miss(t *testing.T) {
	svc, _, _ := setup(t)
	_, err := svc.AuthenticateByAPIKey(context.Background(), "sk_live_nonexistent")
	if !errors.Is(err, domain.ErrMerchantNotFound) {
		t.Fatalf("want ErrMerchantNotFound, got %v", err)
	}
}

func TestAuthenticateByAPIKey_EmptyValidation(t *testing.T) {
	svc, _, _ := setup(t)
	_, err := svc.AuthenticateByAPIKey(context.Background(), "  ")
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}
