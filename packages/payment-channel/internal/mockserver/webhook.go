package mockserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// dispatcher posts a webhook back to the adapter after an optional delay. The
// signer hook lets a channel-specific handler add signature/auth headers that
// match what the real channel would send (so the adapter's ParseWebhook +
// signature-verify paths run under realistic conditions).
type dispatcher struct {
	httpClient *http.Client
	logger     Logger
	webhookDelay time.Duration
}

// signFn mutates the outgoing request (adds headers, etc.) to emulate what the
// real channel's webhook would look like. It runs synchronously right before
// sending.
type signFn func(req *http.Request, body []byte)

func newDispatcher(logger Logger, delay time.Duration) *dispatcher {
	return &dispatcher{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		logger:     logger,
		webhookDelay: delay,
	}
}

// fire schedules a webhook POST. If the dispatcher's delay is > 0 the POST
// runs in a goroutine after the delay; otherwise it runs inline. Errors are
// logged but never returned to the caller — real channels do not block on
// webhook delivery.
func (d *dispatcher) fire(ctx context.Context, url string, body any, sign signFn) {
	if url == "" {
		d.logger.Warn("mockserver: webhook skipped, no url")
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		d.logger.Warn("mockserver: webhook marshal failed: " + err.Error())
		return
	}
	send := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			d.logger.Warn("mockserver: webhook build failed: " + err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "payment-channel-mock/1")
		if sign != nil {
			sign(req, raw)
		}
		resp, err := d.httpClient.Do(req)
		if err != nil {
			d.logger.Warn("mockserver: webhook send failed: " + err.Error())
			return
		}
		_ = resp.Body.Close()
		d.logger.Info("mockserver: webhook delivered", "url", url, "status", resp.StatusCode)
	}
	if d.webhookDelay <= 0 {
		send()
		return
	}
	go func() {
		t := time.NewTimer(d.webhookDelay)
		defer t.Stop()
		select {
		case <-t.C:
			send()
		case <-ctx.Done():
		}
	}()
}
