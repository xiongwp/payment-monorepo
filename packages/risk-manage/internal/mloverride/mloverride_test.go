package mloverride

import (
	"sync"
	"testing"
	"time"
)

func TestStore_DefaultIsInactive(t *testing.T) {
	s := New()
	o := s.Get()
	if o.IsActive() {
		t.Fatal("default should be inactive")
	}
	if o.Disabled || o.ForceScore != 0 {
		t.Fatalf("default should be zero values; got %+v", o)
	}
}

func TestStore_SetAndGet(t *testing.T) {
	s := New()
	s.Set(Override{Disabled: true, Reason: "drift", SetBy: "alice"})
	o := s.Get()
	if !o.IsActive() {
		t.Fatal("expected active after Disabled=true")
	}
	if o.Reason != "drift" || o.SetBy != "alice" {
		t.Fatalf("fields wrong: %+v", o)
	}
	if o.SetAt.IsZero() {
		t.Fatal("SetAt should auto-fill")
	}
}

func TestStore_ForceScoreActiveEvenWhenEnabled(t *testing.T) {
	s := New()
	s.Set(Override{Disabled: false, ForceScore: 0.99})
	if !s.Get().IsActive() {
		t.Fatal("ForceScore != 0 should be active")
	}
}

func TestStore_Clear(t *testing.T) {
	s := New()
	s.Set(Override{Disabled: true, ForceScore: 0.5, Reason: "test"})
	s.Clear("bob")
	o := s.Get()
	if o.IsActive() {
		t.Fatal("expected inactive after Clear")
	}
	if o.SetBy != "bob" || o.Reason != "cleared" {
		t.Fatalf("Clear should preserve actor + set reason; got %+v", o)
	}
}

// hot path 需要 atomic 读：起 100 个 reader + 1 个 writer 验证不 race。
func TestStore_ConcurrentRW(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = s.Get()
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				s.Set(Override{Disabled: true})
				s.Clear("test")
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}

func TestNilStore_Safe(t *testing.T) {
	var s *Store
	// 不 panic
	_ = s.Get()
	s.Set(Override{})
	s.Clear("x")
}
