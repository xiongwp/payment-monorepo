// Package mockserver provides an in-process HTTP server that simulates PH
// payment channels (GCash, Maya, GrabPay, PayMongo, Xendit, Dragonpay, and
// banking/wallet partners). It is intended for local dev, CI e2e tests, and
// sandbox training — not for production traffic.
//
// Each adapter can point at this mock via its Config.BaseURL. Channels that
// deliver async webhooks (GCash, Maya, Dragonpay, Shopeepay, …) will POST back
// to the NotifyURL configured on the adapter.
package mockserver

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// Scenario controls how the mock responds to a Charge/Refund call. The client
// (adapter) influences the scenario in three ways, evaluated in order:
//  1. explicit `X-Mock-Scenario` header
//  2. amount-based convention (useful for e2e test suites)
//  3. default = success
type Scenario int

const (
	ScenarioSuccess Scenario = iota
	ScenarioRequiresAction
	ScenarioFailCardDeclined
	ScenarioFailInsufficientFunds
	ScenarioFailRiskBlocked
	ScenarioFailChannelUnavailable
	ScenarioAsyncSuccess // returns processing, fires succeeded webhook after WebhookDelay
	ScenarioAsyncFail    // returns processing, fires failed webhook after WebhookDelay
	ScenarioTimeout      // sleeps past adapter timeout
)

func (s Scenario) String() string {
	switch s {
	case ScenarioSuccess:
		return "success"
	case ScenarioRequiresAction:
		return "requires_action"
	case ScenarioFailCardDeclined:
		return "fail_card_declined"
	case ScenarioFailInsufficientFunds:
		return "fail_insufficient_funds"
	case ScenarioFailRiskBlocked:
		return "fail_risk_blocked"
	case ScenarioFailChannelUnavailable:
		return "fail_channel_unavailable"
	case ScenarioAsyncSuccess:
		return "async_success"
	case ScenarioAsyncFail:
		return "async_fail"
	case ScenarioTimeout:
		return "timeout"
	}
	return "unknown"
}

// pickScenario resolves a Scenario from the incoming request. header wins
// over amount; amount wins over default.
//
// Amount convention (cents / centavos):
//
//	amount == 1    → ScenarioSuccess               (sanity check)
//	amount == 2    → ScenarioRequiresAction
//	amount == 3    → ScenarioFailCardDeclined
//	amount == 4    → ScenarioFailInsufficientFunds
//	amount == 5    → ScenarioFailRiskBlocked
//	amount == 6    → ScenarioFailChannelUnavailable
//	amount == 7    → ScenarioAsyncSuccess
//	amount == 8    → ScenarioAsyncFail
//	amount == 9    → ScenarioTimeout
//	else           → ScenarioSuccess
//
// Real e2e tests should set the header explicitly; amount convention is for
// curl-driven smoke tests.
func pickScenario(header string, amount int64) Scenario {
	switch strings.ToLower(strings.TrimSpace(header)) {
	case "success":
		return ScenarioSuccess
	case "requires_action", "ra":
		return ScenarioRequiresAction
	case "fail_card_declined", "card_declined":
		return ScenarioFailCardDeclined
	case "fail_insufficient_funds", "insufficient_funds":
		return ScenarioFailInsufficientFunds
	case "fail_risk_blocked", "risk_blocked":
		return ScenarioFailRiskBlocked
	case "fail_channel_unavailable", "channel_unavailable":
		return ScenarioFailChannelUnavailable
	case "async_success":
		return ScenarioAsyncSuccess
	case "async_fail":
		return ScenarioAsyncFail
	case "timeout":
		return ScenarioTimeout
	}
	switch amount {
	case 1:
		return ScenarioSuccess
	case 2:
		return ScenarioRequiresAction
	case 3:
		return ScenarioFailCardDeclined
	case 4:
		return ScenarioFailInsufficientFunds
	case 5:
		return ScenarioFailRiskBlocked
	case 6:
		return ScenarioFailChannelUnavailable
	case 7:
		return ScenarioAsyncSuccess
	case 8:
		return ScenarioAsyncFail
	case 9:
		return ScenarioTimeout
	}
	return ScenarioSuccess
}

// randomRef returns a short-prefixed random id suitable for channel reference
// numbers (e.g. GCash paymentId, PayMongo src_…).
func randomRef(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
