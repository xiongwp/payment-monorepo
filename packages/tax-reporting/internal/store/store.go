// Package store — tax-reporting 持久化.
//
// 三类数据:
//   1. payout_events       (源源不断, append-only)
//   2. merchant_tax_profile (W-9 / W-8 / VAT 注册信息)
//   3. annual_aggregate     (按 (merchant, year, jurisdiction) 滚动汇总)
//   4. tax_forms           (生成的 1099-K / W-8 PDF 引用)
//
// memory impl 给 dev; 生产分库分表 by merchant_id.

package store

import (
	"errors"
	"sort"
	"sync"
	"time"

	"reconcile-system/packages/tax-reporting/internal/domain"
)

var ErrNotFound = errors.New("not found")

type Store interface {
	// PayoutEvent — idempotent by event_id
	AppendPayout(e domain.PayoutEvent) error
	ListPayouts(merchantID string, year int) ([]domain.PayoutEvent, error)

	// MerchantTaxProfile
	UpsertProfile(p domain.MerchantTaxProfile) error
	GetProfile(merchantID string) (domain.MerchantTaxProfile, error)

	// Aggregate
	UpsertAggregate(a domain.Aggregate) error
	GetAggregate(merchantID string, year int, jurisdiction string) (domain.Aggregate, error)
	ListAggregates(year int, jurisdiction string) ([]domain.Aggregate, error)

	// TaxForm
	SaveForm(f domain.TaxForm) error
	GetForm(formID string) (domain.TaxForm, error)
	ListFormsByMerchant(merchantID string) ([]domain.TaxForm, error)
}

type MemStore struct {
	mu       sync.RWMutex
	events   map[string]domain.PayoutEvent       // event_id → event
	byMY     map[string][]string                 // merchant|year → []event_id
	profiles map[string]domain.MerchantTaxProfile
	aggs     map[string]domain.Aggregate         // merchant|year|jurisdiction → agg
	forms    map[string]domain.TaxForm           // form_id → form
	byMerchantForms map[string][]string          // merchant → form_ids
}

func NewMemStore() *MemStore {
	return &MemStore{
		events:          make(map[string]domain.PayoutEvent),
		byMY:            make(map[string][]string),
		profiles:        make(map[string]domain.MerchantTaxProfile),
		aggs:            make(map[string]domain.Aggregate),
		forms:           make(map[string]domain.TaxForm),
		byMerchantForms: make(map[string][]string),
	}
}

func myKey(m string, y int) string {
	return m + "|" + itos(y)
}

func aggKey(m string, y int, j string) string {
	return m + "|" + itos(y) + "|" + j
}

func itos(y int) string {
	// 简单, 不引 strconv (减少 import)
	if y == 0 {
		return "0"
	}
	neg := false
	if y < 0 {
		neg = true
		y = -y
	}
	buf := [16]byte{}
	i := len(buf)
	for y > 0 {
		i--
		buf[i] = byte('0' + y%10)
		y /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func (s *MemStore) AppendPayout(e domain.PayoutEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.events[e.EventID]; ok {
		return nil // idempotent
	}
	s.events[e.EventID] = e
	k := myKey(e.MerchantID, e.OccurredAt.Year())
	s.byMY[k] = append(s.byMY[k], e.EventID)
	return nil
}

func (s *MemStore) ListPayouts(mid string, year int) ([]domain.PayoutEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.byMY[myKey(mid, year)]
	out := make([]domain.PayoutEvent, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.events[id])
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].OccurredAt.Before(out[j].OccurredAt)
	})
	return out, nil
}

func (s *MemStore) UpsertProfile(p domain.MerchantTaxProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.UpdatedAt = time.Now().UTC()
	s.profiles[p.MerchantID] = p
	return nil
}

func (s *MemStore) GetProfile(mid string) (domain.MerchantTaxProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.profiles[mid]
	if !ok {
		return domain.MerchantTaxProfile{}, ErrNotFound
	}
	return p, nil
}

func (s *MemStore) UpsertAggregate(a domain.Aggregate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.UpdatedAt = time.Now().UTC()
	s.aggs[aggKey(a.MerchantID, a.Year, a.Jurisdiction)] = a
	return nil
}

func (s *MemStore) GetAggregate(mid string, year int, j string) (domain.Aggregate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.aggs[aggKey(mid, year, j)]
	if !ok {
		return domain.Aggregate{}, ErrNotFound
	}
	return a, nil
}

func (s *MemStore) ListAggregates(year int, j string) ([]domain.Aggregate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.Aggregate, 0)
	for _, a := range s.aggs {
		if a.Year == year && (j == "" || a.Jurisdiction == j) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, jj int) bool {
		return out[i].TotalGross > out[jj].TotalGross
	})
	return out, nil
}

func (s *MemStore) SaveForm(f domain.TaxForm) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forms[f.FormID] = f
	s.byMerchantForms[f.MerchantID] = append(s.byMerchantForms[f.MerchantID], f.FormID)
	return nil
}

func (s *MemStore) GetForm(formID string) (domain.TaxForm, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.forms[formID]
	if !ok {
		return domain.TaxForm{}, ErrNotFound
	}
	return f, nil
}

func (s *MemStore) ListFormsByMerchant(mid string) ([]domain.TaxForm, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.byMerchantForms[mid]
	out := make([]domain.TaxForm, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.forms[id])
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Year > out[j].Year
	})
	return out, nil
}
