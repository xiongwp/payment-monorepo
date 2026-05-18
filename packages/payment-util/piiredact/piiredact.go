// Package piiredact provides reflection-based PII redaction utilities for
// logs, audit events, and any other "untrusted" data destinations.
//
// Why this package exists:
//   - Each service used to maintain its own hardcoded sensitive-field list
//     (order-core had ~10, payment-channel had ~5, others had none) — easy to
//     forget a field when adding a new one.
//   - Reflection-based + tag-driven approach lets the struct author opt in by
//     adding `pii:"mask"` / `pii:"drop"` to the field, and the redactor handles
//     nested structs/maps/slices uniformly.
//   - The package also exposes name-based heuristics for cases where you
//     cannot add tags (e.g. arbitrary map[string]any from JSON unmarshal).
//
// Three layers:
//
//  1. Tag-driven (highest fidelity)     — Add `pii:"mask"` to a struct field.
//  2. Name-heuristic (default fallback) — Field/key name matched against a
//     curated list (card_number, pan, cvv, email, phone, password, …).
//  3. Value-heuristic (last resort)     — Card-number Luhn detection for
//     strings that look like 13–19 digit runs.
//
// Behavior on match:
//   - "mask": replace with safe summary (e.g. "411111**1111" or "***").
//   - "drop": replace with literal "<redacted>".
//
// Threading: all exported funcs are goroutine-safe; the redactor itself is
// immutable after construction.
package piiredact

import (
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// Action controls what happens to a matched field.
type Action int

const (
	ActionMask Action = iota // partial reveal (e.g. card last4, email domain)
	ActionDrop               // replace with "<redacted>"
)

// FieldRule maps a field-name regex / literal to an Action.
type FieldRule struct {
	// Match (lowercased) — exact match or substring depending on Substring.
	Match     string
	Substring bool   // true: substring match against field name; false: exact
	Action    Action
}

// DefaultFieldRules is the curated baseline. Services should call Default()
// to get a redactor with these rules, then layer additional service-specific
// rules via With().
//
// Naming convention: match on lowercased field name. Substring matches are
// used for prefixes/suffixes (e.g. "_pan" matches "card_pan", "buyer_pan").
var DefaultFieldRules = []FieldRule{
	// Card / payment instrument
	{Match: "pan", Action: ActionMask},
	{Match: "card_number", Action: ActionMask},
	{Match: "cardnumber", Action: ActionMask},
	{Match: "card_no", Action: ActionMask},
	{Match: "cvv", Action: ActionDrop},
	{Match: "cvc", Action: ActionDrop},
	{Match: "cvn", Action: ActionDrop},
	{Match: "card_holder", Action: ActionMask},
	{Match: "cardholder", Action: ActionMask},
	{Match: "expiry", Action: ActionDrop},
	{Match: "expiration", Action: ActionDrop},
	{Match: "track1", Action: ActionDrop},
	{Match: "track2", Action: ActionDrop},
	{Match: "track3", Action: ActionDrop},
	{Match: "pin", Action: ActionDrop, Substring: true}, // pin / pin_block etc.

	// PII
	{Match: "email", Action: ActionMask, Substring: true},
	{Match: "phone", Action: ActionMask, Substring: true},
	{Match: "mobile", Action: ActionMask},
	{Match: "ssn", Action: ActionDrop, Substring: true},
	{Match: "passport", Action: ActionDrop},
	{Match: "id_number", Action: ActionDrop},
	{Match: "id_card", Action: ActionDrop},
	{Match: "tax_id", Action: ActionDrop},
	{Match: "first_name", Action: ActionMask},
	{Match: "last_name", Action: ActionMask},
	{Match: "full_name", Action: ActionMask},

	// Auth / secret
	{Match: "password", Action: ActionDrop, Substring: true},
	{Match: "passwd", Action: ActionDrop},
	{Match: "secret", Action: ActionDrop, Substring: true},
	{Match: "token", Action: ActionDrop, Substring: true},
	{Match: "api_key", Action: ActionDrop, Substring: true},
	{Match: "apikey", Action: ActionDrop, Substring: true},
	{Match: "private_key", Action: ActionDrop, Substring: true},
	{Match: "session_key", Action: ActionDrop, Substring: true},
	{Match: "auth", Action: ActionDrop, Substring: true},
	{Match: "credential", Action: ActionDrop, Substring: true},
	{Match: "otp", Action: ActionDrop, Substring: true},

	// Banking
	{Match: "iban", Action: ActionMask},
	{Match: "bic", Action: ActionDrop},
	{Match: "swift", Action: ActionDrop},
	{Match: "account_number", Action: ActionMask},
	{Match: "routing_number", Action: ActionDrop},
}

// Redactor is the entry point. Built once at process start and used across
// all log/audit sites; threadsafe.
type Redactor struct {
	rules []FieldRule
	// fastExact: exact-match rules in a map for O(1) lookup; substring rules
	// remain in slice (small N).
	fastExact map[string]Action
	substr    []FieldRule
	// LuhnCheck enables string-value heuristic (any 13-19 digit run that
	// passes Luhn → mask). Off by default (perf cost on big payloads).
	LuhnCheck bool
}

// Default returns a Redactor with DefaultFieldRules + LuhnCheck enabled.
func Default() *Redactor { return New(DefaultFieldRules, true) }

// New constructs a Redactor with explicit rules.
func New(rules []FieldRule, luhnCheck bool) *Redactor {
	r := &Redactor{rules: rules, LuhnCheck: luhnCheck}
	r.fastExact = make(map[string]Action, len(rules))
	for _, rule := range rules {
		if rule.Substring {
			r.substr = append(r.substr, rule)
		} else {
			r.fastExact[strings.ToLower(rule.Match)] = rule.Action
		}
	}
	return r
}

// With returns a new Redactor with additional rules appended (immutable).
func (r *Redactor) With(extra ...FieldRule) *Redactor {
	merged := make([]FieldRule, 0, len(r.rules)+len(extra))
	merged = append(merged, r.rules...)
	merged = append(merged, extra...)
	out := New(merged, r.LuhnCheck)
	return out
}

// matchAction looks up the action for a field name. Returns (action, true) if
// matched, (0, false) otherwise.
func (r *Redactor) matchAction(name string) (Action, bool) {
	if name == "" {
		return 0, false
	}
	lc := strings.ToLower(name)
	if a, ok := r.fastExact[lc]; ok {
		return a, true
	}
	for _, rule := range r.substr {
		if strings.Contains(lc, strings.ToLower(rule.Match)) {
			return rule.Action, true
		}
	}
	return 0, false
}

// Redact returns a deep-copied version of v with PII fields masked / dropped.
//
// Supported types:
//   - struct: recurse on fields; honor `pii:"mask"` / `pii:"drop"` tag.
//   - map[string]any / map[string]string: recurse on values; key name drives action.
//   - []any: recurse on elements.
//   - string: if LuhnCheck enabled, scan for card-like runs and mask.
//   - everything else: returned as-is.
//
// The input is never mutated.
func (r *Redactor) Redact(v any) any {
	if v == nil {
		return nil
	}
	return r.redactValue(reflect.ValueOf(v), "")
}

// RedactString convenience — runs Luhn check on a free-form string.
func (r *Redactor) RedactString(s string) string {
	if !r.LuhnCheck {
		return s
	}
	return scrubLuhn(s)
}

// RedactJSONString scrubs a JSON-encoded string (e.g. output of protojson.Marshal)
// by:
//  1. matching field-name rules — keys in DefaultFieldRules / custom rules get
//     their value replaced with "<redacted>" (Action=Drop) or masked (Action=Mask).
//  2. Luhn-scanning the whole string (catches card numbers in any free-form text).
//
// This is a string-level scrub, not a JSON parser; it's robust to malformed JSON
// and avoids the cost of unmarshal+remarshal. Trade-off: nested struct paths
// aren't context-aware (a "password" field anywhere in the JSON gets the same
// treatment regardless of nesting).
//
// Use this in gRPC LoggingInterceptors that have already serialized the proto
// to JSON via protojson.Marshal. For struct-level redaction (full reflection
// path), use Redact(v).
func (r *Redactor) RedactJSONString(s string) string {
	if s == "" {
		return s
	}
	// 1. field-name scrub. We scan for both `"key":"value"` and `"key": "value"` forms.
	for key, action := range r.fastExact {
		s = scrubJSONField(s, key, action)
	}
	for _, rule := range r.substr {
		// For substring rules we walk all JSON keys and check each.
		s = scrubJSONFieldSubstr(s, strings.ToLower(rule.Match), rule.Action)
	}
	// 2. Luhn fallback over whatever remains.
	if r.LuhnCheck {
		s = scrubLuhn(s)
	}
	return s
}

// scrubJSONField replaces the value of `"<key>": "..."` with action-appropriate
// placeholder. Case-insensitive key matching; tolerant of whitespace + camelCase
// vs snake_case forms (we generate both variants).
func scrubJSONField(s, key string, action Action) string {
	if key == "" {
		return s
	}
	// We generate camelCase + snake_case variants to catch both protojson naming styles.
	variants := []string{key, snakeToCamel(key), camelToSnake(key)}
	seen := map[string]bool{}
	for _, v := range variants {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		s = replaceJSONStringField(s, v, action)
	}
	return s
}

// scrubJSONFieldSubstr handles substring-match rules by walking JSON keys.
// More expensive than fastExact path; only run for the ~5-10 substring rules.
func scrubJSONFieldSubstr(s, needle string, action Action) string {
	// Find every `"<key>":"<value>"` where <key> contains needle.
	out := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		// Look for a key-start `"`.
		if s[i] != '"' {
			out = append(out, s[i])
			i++
			continue
		}
		// find matching close quote
		j := i + 1
		for j < len(s) && s[j] != '"' {
			if s[j] == '\\' && j+1 < len(s) {
				j += 2
				continue
			}
			j++
		}
		if j >= len(s) {
			out = append(out, s[i:]...)
			break
		}
		key := strings.ToLower(s[i+1 : j])
		// k follows by either `:` or `":` after optional whitespace
		k := j + 1
		for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
		if k >= len(s) || s[k] != ':' {
			out = append(out, s[i:j+1]...)
			i = j + 1
			continue
		}
		// emit `"key":`
		out = append(out, s[i:k+1]...)
		i = k + 1
		// skip whitespace
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			out = append(out, s[i])
			i++
		}
		// only string values are scrubbed (numbers/objects/arrays left alone).
		if i >= len(s) || s[i] != '"' {
			continue
		}
		// find value end
		ve := i + 1
		for ve < len(s) && s[ve] != '"' {
			if s[ve] == '\\' && ve+1 < len(s) {
				ve += 2
				continue
			}
			ve++
		}
		if ve >= len(s) {
			out = append(out, s[i:]...)
			break
		}
		if strings.Contains(key, needle) {
			placeholder := applyAction(s[i+1:ve], action)
			out = append(out, '"')
			out = append(out, []byte(placeholder)...)
			out = append(out, '"')
		} else {
			out = append(out, s[i:ve+1]...)
		}
		i = ve + 1
	}
	return string(out)
}

// replaceJSONStringField replaces the string value of a specific key.
// Matches `"<key>":"<value>"` and `"<key>": "<value>"` (with whitespace).
func replaceJSONStringField(s, key string, action Action) string {
	keyPat := `"` + key + `"`
	out := s
	for {
		idx := strings.Index(out, keyPat)
		if idx < 0 {
			break
		}
		// after key: optional ws + `:` + optional ws + `"value"`
		k := idx + len(keyPat)
		for k < len(out) && (out[k] == ' ' || out[k] == '\t') {
			k++
		}
		if k >= len(out) || out[k] != ':' {
			// not a key — could be a value match; skip past this idx
			out = out[:idx+1] + replaceJSONStringField(out[idx+1:], key, action)
			break
		}
		k++ // past ':'
		for k < len(out) && (out[k] == ' ' || out[k] == '\t') {
			k++
		}
		if k >= len(out) || out[k] != '"' {
			// non-string value (number/object/bool); leave it; advance past.
			out = out[:k] + replaceJSONStringField(out[k:], key, action)
			break
		}
		// k is at opening quote of value
		valStart := k + 1
		valEnd := valStart
		for valEnd < len(out) && out[valEnd] != '"' {
			if out[valEnd] == '\\' && valEnd+1 < len(out) {
				valEnd += 2
				continue
			}
			valEnd++
		}
		if valEnd >= len(out) {
			break
		}
		placeholder := applyAction(out[valStart:valEnd], action)
		out = out[:valStart] + placeholder + out[valEnd:]
		// advance past the (potentially shorter) replacement
		nextIdx := valStart + len(placeholder) + 1
		if nextIdx >= len(out) {
			break
		}
		// recurse over rest
		out = out[:nextIdx] + replaceJSONStringField(out[nextIdx:], key, action)
		break
	}
	return out
}

// snakeToCamel "card_number" → "cardNumber".
func snakeToCamel(s string) string {
	if !strings.Contains(s, "_") {
		return ""
	}
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) > 0 {
			parts[i] = strings.ToUpper(string(parts[i][0])) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// camelToSnake "cardNumber" → "card_number".
func camelToSnake(s string) string {
	var b strings.Builder
	hasUpper := false
	for i, c := range s {
		if c >= 'A' && c <= 'Z' {
			hasUpper = true
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(c + ('a' - 'A'))
		} else {
			b.WriteRune(c)
		}
	}
	if !hasUpper {
		return ""
	}
	return b.String()
}

// RedactJSONString — process-wide default convenience.
func RedactJSONString(s string) string { return GetDefault().RedactJSONString(s) }

func (r *Redactor) redactValue(v reflect.Value, fieldName string) any {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return r.redactValue(v.Elem(), fieldName)
	case reflect.Struct:
		return r.redactStruct(v)
	case reflect.Map:
		return r.redactMap(v)
	case reflect.Slice, reflect.Array:
		out := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ {
			out[i] = r.redactValue(v.Index(i), fieldName)
		}
		return out
	case reflect.String:
		// Field-name match wins; otherwise check Luhn on the string content.
		if action, ok := r.matchAction(fieldName); ok {
			return applyAction(v.String(), action)
		}
		if r.LuhnCheck {
			return scrubLuhn(v.String())
		}
		return v.String()
	default:
		// numbers / bool / etc — leave as-is
		if v.CanInterface() {
			return v.Interface()
		}
		return nil
	}
}

func (r *Redactor) redactStruct(v reflect.Value) map[string]any {
	t := v.Type()
	out := make(map[string]any, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := jsonOrFieldName(f)
		fv := v.Field(i)

		// 1. struct tag takes precedence
		if tag := f.Tag.Get("pii"); tag != "" {
			switch tag {
			case "mask":
				out[name] = applyAction(stringify(fv), ActionMask)
				continue
			case "drop":
				out[name] = "<redacted>"
				continue
			case "skip", "-":
				// passthrough — author certified safe
				out[name] = safeInterface(fv)
				continue
			}
		}
		// 2. name match
		if action, ok := r.matchAction(name); ok {
			out[name] = applyAction(stringify(fv), action)
			continue
		}
		// 3. recurse
		out[name] = r.redactValue(fv, name)
	}
	return out
}

func (r *Redactor) redactMap(v reflect.Value) map[string]any {
	out := make(map[string]any, v.Len())
	iter := v.MapRange()
	for iter.Next() {
		k := iter.Key()
		var keyStr string
		if k.Kind() == reflect.String {
			keyStr = k.String()
		} else {
			keyStr = stringify(k)
		}
		val := iter.Value()
		if action, ok := r.matchAction(keyStr); ok {
			out[keyStr] = applyAction(stringify(val), action)
			continue
		}
		out[keyStr] = r.redactValue(val, keyStr)
	}
	return out
}

// applyAction renders the value per requested action.
func applyAction(s string, a Action) string {
	switch a {
	case ActionDrop:
		return "<redacted>"
	case ActionMask:
		return maskValue(s)
	}
	return s
}

// maskValue produces a "safe" partial reveal.
//
// Strategy:
//   - len ≤ 4   → "***"
//   - 5..10     → first 1 + "***" + last 1
//   - 11..      → first 2 + "***" + last 2
//   - email-like (contains "@") → first letter + "***@" + domain
//   - card-like (13-19 digits)  → first 6 + "***" + last 4 (PCI standard "BIN+last4")
func maskValue(s string) string {
	if s == "" {
		return ""
	}
	if strings.Contains(s, "@") {
		at := strings.IndexByte(s, '@')
		if at > 0 {
			local := s[:at]
			domain := s[at:]
			if len(local) == 0 {
				return s
			}
			return string(local[0]) + "***" + domain
		}
	}
	if isAllDigits(s) && len(s) >= 13 && len(s) <= 19 {
		return s[:6] + strings.Repeat("*", len(s)-10) + s[len(s)-4:]
	}
	n := len(s)
	if n <= 4 {
		return "***"
	}
	if n <= 10 {
		return string(s[0]) + "***" + string(s[n-1])
	}
	return s[:2] + "***" + s[n-2:]
}

// scrubLuhn finds runs of 13-19 digits in s and masks those that pass Luhn.
// Used when LuhnCheck=true to catch card numbers in free-form strings (e.g.
// gateway raw response bodies).
func scrubLuhn(s string) string {
	if len(s) < 13 {
		return s
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		if !isDigit(s[i]) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		run := s[i:j]
		if len(run) >= 13 && len(run) <= 19 && luhn(run) {
			b.WriteString(run[:6])
			b.WriteString(strings.Repeat("*", len(run)-10))
			b.WriteString(run[len(run)-4:])
		} else {
			b.WriteString(run)
		}
		i = j
	}
	return b.String()
}

func luhn(s string) bool {
	sum := 0
	alt := false
	for i := len(s) - 1; i >= 0; i-- {
		d := int(s[i] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

func isDigit(b byte) bool  { return b >= '0' && b <= '9' }
func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

// jsonOrFieldName prefers the JSON tag name, falls back to field name.
func jsonOrFieldName(f reflect.StructField) string {
	if tag := f.Tag.Get("json"); tag != "" && tag != "-" {
		if idx := strings.IndexByte(tag, ','); idx >= 0 {
			tag = tag[:idx]
		}
		if tag != "" {
			return tag
		}
	}
	return f.Name
}

func stringify(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return stringify(v.Elem())
	case reflect.String:
		return v.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	}
	if v.CanInterface() {
		// give up — caller probably wants the type-aware path, not a stringify here.
		return ""
	}
	return ""
}

func safeInterface(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	if v.CanInterface() {
		return v.Interface()
	}
	return nil
}

// ─── Process-wide default instance ────────────────────────────────────

var (
	defaultMu sync.RWMutex
	defaultR  = Default()
)

// SetDefault swaps the process-wide default Redactor. Tests / services can
// install custom rules at startup.
func SetDefault(r *Redactor) {
	defaultMu.Lock()
	defaultR = r
	defaultMu.Unlock()
}

// GetDefault returns the process-wide default.
func GetDefault() *Redactor {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultR
}

// Redact uses the process-wide default.
func Redact(v any) any { return GetDefault().Redact(v) }

// RedactString uses the process-wide default.
func RedactString(s string) string { return GetDefault().RedactString(s) }
