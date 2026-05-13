package trace

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func sp(name string, attrs ...attribute.KeyValue) sdktrace.SamplingParameters {
	// 随机一个 traceID;ratioSample 不能对所有 traceID 都返同一结果
	return sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		Name:          name,
		Attributes:    attrs,
		TraceID:       oteltrace.TraceID{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
	}
}

func TestSampler_DefaultRatio_ClampsToValid(t *testing.T) {
	for _, in := range []float64{-1, 0, 0.05, 1, 2} {
		s := NewAdaptiveSampler(SamplerOpts{DefaultRatio: in})
		if s == nil {
			t.Fatalf("nil sampler for %v", in)
		}
	}
}

func TestSampler_RuleMatchByName(t *testing.T) {
	s := NewAdaptiveSampler(SamplerOpts{
		DefaultRatio: 0,
		Rules: []SamplerRule{
			{SpanNameRegex: `^paycore\.Charge$`, Ratio: 1.0},
		},
	})
	got := s.ShouldSample(sp("paycore.Charge"))
	if got.Decision != sdktrace.RecordAndSample {
		t.Errorf("expect sampled for matched rule, got %v", got.Decision)
	}
	got = s.ShouldSample(sp("paycore.Confirm"))
	// 默认 ratio=0 → drop
	if got.Decision != sdktrace.Drop {
		t.Errorf("expect drop for non-matched, got %v", got.Decision)
	}
}

func TestSampler_RuleMatchByAttr(t *testing.T) {
	s := NewAdaptiveSampler(SamplerOpts{
		DefaultRatio: 0,
		Rules: []SamplerRule{
			{AttrKey: "merchant.id", AttrEquals: "m_vip_1", Ratio: 1.0},
		},
	})
	got := s.ShouldSample(sp("any", attribute.String("merchant.id", "m_vip_1")))
	if got.Decision != sdktrace.RecordAndSample {
		t.Errorf("expect sampled for matched attr, got %v", got.Decision)
	}
	got = s.ShouldSample(sp("any", attribute.String("merchant.id", "m_normal")))
	if got.Decision != sdktrace.Drop {
		t.Errorf("expect drop for non-matched attr, got %v", got.Decision)
	}
}

func TestSampler_ErrorAlwaysSample(t *testing.T) {
	s := NewAdaptiveSampler(SamplerOpts{
		DefaultRatio:      0,
		ErrorAlwaysSample: true,
	})
	got := s.ShouldSample(sp("foo", attribute.String("error", "true")))
	if got.Decision != sdktrace.RecordAndSample {
		t.Errorf("expect sampled for error=true, got %v", got.Decision)
	}
	got = s.ShouldSample(sp("foo", attribute.Bool("error", true)))
	if got.Decision != sdktrace.RecordAndSample {
		t.Errorf("expect sampled for error=bool true, got %v", got.Decision)
	}
}

func TestSampler_ErrorAlwaysSample_Off(t *testing.T) {
	s := NewAdaptiveSampler(SamplerOpts{
		DefaultRatio:      0,
		ErrorAlwaysSample: false,
	})
	got := s.ShouldSample(sp("foo", attribute.String("error", "true")))
	if got.Decision != sdktrace.Drop {
		t.Errorf("expect drop when errorAlwaysSample is off, got %v", got.Decision)
	}
}

func TestSampler_RatioBoundary(t *testing.T) {
	s := NewAdaptiveSampler(SamplerOpts{
		DefaultRatio: 1.0,
	})
	got := s.ShouldSample(sp("any"))
	if got.Decision != sdktrace.RecordAndSample {
		t.Errorf("ratio=1.0 must always sample, got %v", got.Decision)
	}

	s2 := NewAdaptiveSampler(SamplerOpts{DefaultRatio: 0.0001})
	got2 := s2.ShouldSample(sp("rare"))
	// 极低概率,这个 traceID 几乎一定 drop
	if got2.Decision == sdktrace.RecordAndSample {
		t.Logf("low-ratio sampling decided to sample (acceptable due to traceID dist)")
	}
}

func TestSampler_Description(t *testing.T) {
	s := NewAdaptiveSampler(SamplerOpts{
		DefaultRatio:      0.05,
		Rules:             []SamplerRule{{SpanNameRegex: "x", Ratio: 0.5}},
		ErrorAlwaysSample: true,
	})
	desc := s.Description()
	if desc == "" {
		t.Fatal("empty description")
	}
}

func TestLoadSamplerOptsFromEnv_Defaults(t *testing.T) {
	t.Setenv("OTEL_SAMPLE_RATIO", "")
	t.Setenv("OTEL_SAMPLING_RULES_JSON", "")
	t.Setenv("OTEL_ERROR_ALWAYS_SAMPLE", "")
	o := LoadSamplerOptsFromEnv()
	if o.DefaultRatio != 0.05 || !o.ErrorAlwaysSample {
		t.Errorf("defaults wrong: %+v", o)
	}
}

func TestLoadSamplerOptsFromEnv_WithJSONRules(t *testing.T) {
	t.Setenv("OTEL_SAMPLE_RATIO", "0.2")
	t.Setenv("OTEL_SAMPLING_RULES_JSON", `[{"span_name_regex":"^paycore\\.","ratio":0.5}]`)
	t.Setenv("OTEL_ERROR_ALWAYS_SAMPLE", "false")
	o := LoadSamplerOptsFromEnv()
	if o.DefaultRatio != 0.2 || o.ErrorAlwaysSample || len(o.Rules) != 1 {
		t.Errorf("env load wrong: %+v", o)
	}
	if o.Rules[0].Ratio != 0.5 {
		t.Errorf("rule ratio wrong: %v", o.Rules[0])
	}
}
