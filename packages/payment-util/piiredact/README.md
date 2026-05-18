# piiredact

Reflection-based PII redaction for logs, audit events, and any "untrusted" data
destination across the payment platform.

## Quick start

```go
import "github.com/xiongwp/payment-util/piiredact"

// gRPC server: drop into the interceptor chain
srv := grpc.NewServer(
    grpc.UnaryInterceptor(piiredact.LoggingInterceptor(log, piiredact.LoggingOptions{
        LogPayload: true,
    })),
)

// Ad-hoc log site
log.Info("user form submitted", piiredact.ZapField("form", form))

// Free-form string (raw response body etc.)
log.Debug("channel response", piiredact.ZapStringField("body", rawHTTPBody))
```

## What gets redacted

Three layers, evaluated in order:

1. **Struct tag** (highest fidelity): add `pii:"mask"` / `pii:"drop"` / `pii:"skip"` to the field.
   ```go
   type Charge struct {
       PAN      string `json:"pan" pii:"mask"`     // → "411111******1111"
       CVV      string `json:"cvv" pii:"drop"`     // → "<redacted>"
       OrderID  string `json:"order_id" pii:"skip"` // passthrough (author certified safe)
   }
   ```

2. **Name heuristic** (default fallback): field/key name matched against the
   built-in `DefaultFieldRules` list — covers `pan`, `cvv`, `email`, `phone`,
   `password`, `token`, `api_key`, `iban`, `passport`, etc. (~40 rules).

3. **Value heuristic** (last resort): Luhn-valid 13-19 digit runs in any
   string are masked. Catches card numbers in raw response bodies the author
   didn't anticipate.

## Adding service-specific rules

```go
// At process startup
piiredact.SetDefault(piiredact.Default().With(
    piiredact.FieldRule{Match: "internal_audit_id", Action: piiredact.ActionDrop},
    piiredact.FieldRule{Match: "fingerprint", Substring: true, Action: piiredact.ActionDrop},
))
```

## Mask format

| Input shape | Output |
|---|---|
| 13-19 digits (likely card) | `411111******1111` (BIN + last4, PCI standard) |
| Email | `a***@example.com` |
| ≤ 4 chars | `***` |
| 5-10 chars | `a***z` |
| 11+ chars | `ab***yz` |

## Not in scope

- **Binary blobs** (raw protobuf bytes etc.) — not introspected. Use struct fields, not `[]byte`.
- **Indexed full-text search** — designed for log output, not search-engine sanitization.
- **TLS / transport-level masking** — this is post-handler logging; use mTLS for in-flight protection.

## Performance

- Tag + exact-name match: O(1) map lookup.
- Substring rules: O(N) where N ≈ 20 substring patterns — negligible at log rate.
- Luhn scan: O(M) where M = string length; only triggered when `LuhnCheck=true`.
- Reflection cost: ~5-15μs per redacted struct in benchmarks; acceptable at typical log rates (< 1k QPS access log).

Run benchmarks:
```
go test -bench=. ./piiredact
```

## Migration from hardcoded redact lists

If you have an existing `sensitiveFields := []string{"cardNumber", "apiKey", ...}` in your service:

```diff
- sensitiveFields := map[string]bool{
-     "cardNumber": true,
-     "apiKey": true,
-     "password": true,
- }
- for k := range form {
-     if sensitiveFields[k] {
-         form[k] = "***"
-     }
- }
+ redacted := piiredact.Redact(form)
+ log.Info("form", zap.Any("data", redacted))
```

Coverage will *increase* immediately (the package recognizes ~40 more field
patterns than typical hardcoded lists).
