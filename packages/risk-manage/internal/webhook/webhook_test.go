package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestPublisher_DeliversWithSignature(t *testing.T) {
	type captured struct {
		body []byte
		hdr  http.Header
	}
	gotCh := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotCh <- captured{body: b, hdr: r.Header.Clone()}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := NewMemSubscriptionStore()
	store.Set(&Subscription{
		MerchantID: "m1", URL: srv.URL, Secret: "s3cr3t",
	})
	p := NewPublisher(store, zap.NewNop(), 16, 1, 1)
	defer p.Stop()

	p.Publish(context.Background(), "m1", &Event{
		Type: EventReviewCreated,
		Data: map[string]any{"foo": "bar"},
	})

	select {
	case got := <-gotCh:
		// body is JSON-marshaled Event
		var ev Event
		if err := json.Unmarshal(got.body, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != EventReviewCreated || ev.MerchantID != "m1" {
			t.Fatalf("payload mismatch: %+v", ev)
		}
		// signature
		mac := hmac.New(sha256.New, []byte("s3cr3t"))
		mac.Write(got.body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if got.hdr.Get("X-Risk-Signature") != want {
			t.Fatalf("signature mismatch:\nwant %s\n got %s", want, got.hdr.Get("X-Risk-Signature"))
		}
		if got.hdr.Get("X-Risk-Event-Type") != string(EventReviewCreated) {
			t.Fatalf("event-type header missing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for delivery")
	}
}

func TestPublisher_RetriesOnUpstreamFailure(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := NewMemSubscriptionStore()
	store.Set(&Subscription{MerchantID: "m1", URL: srv.URL, Secret: "x"})
	p := NewPublisher(store, zap.NewNop(), 16, 1, 5)
	defer p.Stop()

	p.Publish(context.Background(), "m1", &Event{Type: EventDecisionDenied})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&attempts) >= 3 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("did not retry to success; attempts=%d", atomic.LoadInt32(&attempts))
}

func TestPublisher_DroppedWhenDisabled(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
	}))
	defer srv.Close()

	store := NewMemSubscriptionStore()
	store.Set(&Subscription{MerchantID: "m1", URL: srv.URL, Secret: "x", Disabled: true})
	p := NewPublisher(store, zap.NewNop(), 16, 1, 1)
	defer p.Stop()

	p.Publish(context.Background(), "m1", &Event{Type: EventReviewCreated})
	time.Sleep(200 * time.Millisecond)
	if atomic.LoadInt32(&attempts) != 0 {
		t.Fatalf("disabled subscription should not deliver, got %d", attempts)
	}
}

func TestPublisher_FilterByEventType(t *testing.T) {
	var got int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&got, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	store := NewMemSubscriptionStore()
	store.Set(&Subscription{
		MerchantID: "m1", URL: srv.URL, Secret: "x",
		Events: []EventType{EventDecisionDenied},
	})
	p := NewPublisher(store, zap.NewNop(), 16, 1, 1)
	defer p.Stop()

	// 不在订阅列表 → drop
	p.Publish(context.Background(), "m1", &Event{Type: EventReviewCreated})
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&got) != 0 {
		t.Fatalf("unsubscribed event should drop, got %d", got)
	}
	// 在订阅列表 → 推
	p.Publish(context.Background(), "m1", &Event{Type: EventDecisionDenied})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&got) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("subscribed event should deliver")
}

// TestSignBody_Stable 让商户实现验签时有参考。
func TestSignBody_Stable(t *testing.T) {
	body := []byte(`{"id":"evt_1","foo":"bar"}`)
	got := signBody("topsecret", body)
	if !strings.HasPrefix(got, "sha256=") {
		t.Fatalf("missing prefix: %s", got)
	}
	// 商户验签等价代码：
	mac := hmac.New(sha256.New, []byte("topsecret"))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("hmac mismatch:\nwant %s\n got %s", want, got)
	}
}

// 让 var unused 检查不抱怨 — _ 引用一下保证 import 不被去掉
var _ = sync.Mutex{}
var _ bytes.Buffer
var _ = fmt.Sprintf
