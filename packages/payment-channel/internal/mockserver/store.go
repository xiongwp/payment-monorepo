package mockserver

import (
	"sync"
	"time"
)

// Payment is a single in-memory payment record stored by the mock. Every
// channel handler converts its native shape into this struct so the core
// store / webhook pipeline can be shared.
type Payment struct {
	Channel        string
	PaymentID      string // our ref (returned to adapter as external_ref_no)
	RequestID      string // adapter's idempotency key / merchant request id
	Amount         int64
	Currency       string
	Status         string // PENDING / SUCCESS / FAILED / REQUIRES_ACTION
	Scenario       Scenario
	FailureCode    string // channel-native code
	RefundedAmount int64
	NotifyURL      string // webhook target the adapter provided
	CreatedAt      time.Time
	Extra          map[string]string
}

// store is a tiny in-memory key/value store keyed by RequestID. Real channels
// also key by their own paymentId; we keep both mapped. Idempotent re-POSTs
// with the same RequestID replay the original response.
type store struct {
	mu     sync.Mutex
	byReq  map[string]*Payment // key: channel + "|" + RequestID
	byPay  map[string]*Payment // key: channel + "|" + PaymentID
}

func newStore() *store {
	return &store{
		byReq: make(map[string]*Payment),
		byPay: make(map[string]*Payment),
	}
}

func reqKey(channel, reqID string) string { return channel + "|" + reqID }
func payKey(channel, payID string) string { return channel + "|" + payID }

func (s *store) loadByReq(channel, reqID string) (*Payment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byReq[reqKey(channel, reqID)]
	return p, ok
}

func (s *store) loadByPay(channel, payID string) (*Payment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byPay[payKey(channel, payID)]
	return p, ok
}

func (s *store) save(p *Payment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byReq[reqKey(p.Channel, p.RequestID)] = p
	s.byPay[payKey(p.Channel, p.PaymentID)] = p
}

// update applies fn under the lock and returns the resulting snapshot.
func (s *store) update(channel, payID string, fn func(*Payment)) (*Payment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byPay[payKey(channel, payID)]
	if !ok {
		return nil, false
	}
	fn(p)
	return p, true
}
