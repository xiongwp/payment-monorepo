package configx

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func newViper(kv map[string]any) *viper.Viper {
	v := viper.New()
	for k, val := range kv {
		v.Set(k, val)
	}
	return v
}

func TestRun_AllPass(t *testing.T) {
	v := newViper(map[string]any{
		"database.meta.dsn": "root:pw@tcp/db",
		"timeouts.default":  "5s",
	})
	err := Run(v,
		Required("database.meta.dsn"),
		PositiveDuration("timeouts.default"),
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestRun_AccumulatesFailures(t *testing.T) {
	v := newViper(map[string]any{
		"timeouts.default": "0",
	})
	err := Run(v,
		Required("database.meta.dsn"),
		PositiveDuration("timeouts.default"),
	)
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "database.meta.dsn") {
		t.Errorf("missing dsn: %s", msg)
	}
	if !strings.Contains(msg, "timeouts.default") {
		t.Errorf("missing timeout: %s", msg)
	}
}

func TestRequired(t *testing.T) {
	if err := Required("a.b")(newViper(nil)); err == nil {
		t.Fatal("missing should fail")
	}
	if err := Required("a.b")(newViper(map[string]any{"a.b": "   "})); err == nil {
		t.Fatal("whitespace should fail")
	}
	if err := Required("a.b")(newViper(map[string]any{"a.b": "x"})); err != nil {
		t.Fatalf("non-empty should pass: %v", err)
	}
}

func TestPositiveDuration(t *testing.T) {
	if err := PositiveDuration("d")(newViper(map[string]any{"d": "1s"})); err != nil {
		t.Fatal("1s should pass")
	}
	if err := PositiveDuration("d")(newViper(map[string]any{"d": "0"})); err == nil {
		t.Fatal("0 should fail")
	}
	if err := PositiveDuration("d")(newViper(map[string]any{"d": "-1s"})); err == nil {
		t.Fatal("negative should fail")
	}
}

func TestNonNegativeInt(t *testing.T) {
	if err := NonNegativeInt("n")(newViper(map[string]any{"n": 0})); err != nil {
		t.Fatal("0 should pass NonNegativeInt")
	}
	if err := NonNegativeInt("n")(newViper(map[string]any{"n": -1})); err == nil {
		t.Fatal("-1 should fail")
	}
}

func TestPositiveInt(t *testing.T) {
	if err := PositiveInt("n")(newViper(map[string]any{"n": 1})); err != nil {
		t.Fatal("1 should pass")
	}
	if err := PositiveInt("n")(newViper(map[string]any{"n": 0})); err == nil {
		t.Fatal("0 should fail PositiveInt")
	}
}

func TestOneOf(t *testing.T) {
	v := newViper(map[string]any{"env": "prod"})
	if err := OneOf("env", "dev", "stg", "prod")(v); err != nil {
		t.Fatal("prod should pass")
	}
	v.Set("env", "xxx")
	if err := OneOf("env", "dev", "stg", "prod")(v); err == nil {
		t.Fatal("xxx should fail")
	}
}

func TestWhen_Skips(t *testing.T) {
	v := newViper(nil)
	// kms.endpoint is not set → When should skip.
	err := When(KeySet("kms.endpoint"), PositiveDuration("kms.rpc_timeout"))(v)
	if err != nil {
		t.Fatalf("When(skipped) should return nil: %v", err)
	}
}

func TestWhen_Runs(t *testing.T) {
	v := newViper(map[string]any{"kms.endpoint": "x:1"})
	// endpoint set → inner check runs; no rpc_timeout → error.
	err := When(KeySet("kms.endpoint"), PositiveDuration("kms.rpc_timeout"))(v)
	if err == nil {
		t.Fatal("When(ran) should surface inner error")
	}
}

func TestDurationAtLeast(t *testing.T) {
	v := newViper(map[string]any{"d": "1s"})
	if err := DurationAtLeast("d", 100*time.Millisecond)(v); err != nil {
		t.Fatal("1s >= 100ms should pass")
	}
	v.Set("d", "50ms")
	if err := DurationAtLeast("d", 100*time.Millisecond)(v); err == nil {
		t.Fatal("50ms < 100ms should fail")
	}
}
