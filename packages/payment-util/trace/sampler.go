// Package trace — sampler.go: 自适应/规则化采样.
//
// 默认行为: head-based fixed ratio (从 env OTEL_SAMPLE_RATIO 读, 默认 0.05).
//
// 规则化采样: 通过环境变量 OTEL_SAMPLING_RULES_JSON 注入,每条规则匹配 span
// 属性 (service.name / http.target / rpc.method / merchant.id / error=true) →
// 对应不同的固定比率;若同时设置 errorAlwaysSample=true (默认 true),则任何
// error=true 的 span 100% 采样,无视 ratio。
//
// 用法:
//
//	tp := sdktrace.NewTracerProvider(
//	    sdktrace.WithSampler(NewAdaptiveSampler(SamplerOpts{
//	        DefaultRatio: 0.05,
//	        Rules: []SamplerRule{
//	            {SpanNameRegex: `^paycore\.Charge$`, Ratio: 0.5},
//	            {AttrKey: "error", AttrEquals: "true", Ratio: 1.0},
//	        },
//	        ErrorAlwaysSample: true,
//	    })),
//	)
package trace

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// SamplerRule 单条采样规则.
type SamplerRule struct {
	// SpanNameRegex 匹配 span 名 (e.g. `paycore\.Charge`, `.*\.Confirm`).
	// 空 → 匹配任何名字.
	SpanNameRegex string `json:"span_name_regex,omitempty"`
	// AttrKey + AttrEquals 匹配某 attribute (e.g. ("error","true"), ("merchant.id","m_vip_1")).
	AttrKey    string `json:"attr_key,omitempty"`
	AttrEquals string `json:"attr_equals,omitempty"`
	// Ratio 这条规则命中的 span 的采样比率 [0,1].
	Ratio float64 `json:"ratio"`

	// 编译后字段
	compiledRegex *regexp.Regexp
}

// SamplerOpts 配置.
type SamplerOpts struct {
	DefaultRatio      float64       `json:"default_ratio"`
	Rules             []SamplerRule `json:"rules,omitempty"`
	ErrorAlwaysSample bool          `json:"error_always_sample,omitempty"`
}

// AdaptiveSampler 实现 sdktrace.Sampler.
type AdaptiveSampler struct {
	defaultSampler sdktrace.Sampler
	rules          []SamplerRule
	errorSample    bool
}

// NewAdaptiveSampler 构造采样器.
func NewAdaptiveSampler(opts SamplerOpts) *AdaptiveSampler {
	if opts.DefaultRatio <= 0 {
		opts.DefaultRatio = 0.05
	}
	if opts.DefaultRatio > 1 {
		opts.DefaultRatio = 1
	}
	compiled := make([]SamplerRule, 0, len(opts.Rules))
	for _, r := range opts.Rules {
		if r.SpanNameRegex != "" {
			re, err := regexp.Compile(r.SpanNameRegex)
			if err == nil {
				r.compiledRegex = re
			}
		}
		if r.Ratio < 0 {
			r.Ratio = 0
		}
		if r.Ratio > 1 {
			r.Ratio = 1
		}
		compiled = append(compiled, r)
	}
	return &AdaptiveSampler{
		defaultSampler: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opts.DefaultRatio)),
		rules:          compiled,
		errorSample:    opts.ErrorAlwaysSample,
	}
}

// ShouldSample 实现 sdktrace.Sampler 接口.
func (s *AdaptiveSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	// 1) parent 已采样 → 跟随
	psc := trace.SpanContextFromContext(p.ParentContext)
	if psc.IsValid() && psc.IsSampled() {
		return sdktrace.SamplingResult{
			Decision:   sdktrace.RecordAndSample,
			Tracestate: psc.TraceState(),
		}
	}

	// 2) error attr → 强采 (运行时挂的 error tag,通常 span end 才挂,但 OTel 也允许
	//    在 start span 时注入。这里检查 attribute 集合)
	if s.errorSample {
		for _, a := range p.Attributes {
			if a.Key == "error" && a.Value.Type() == attribute.STRING && a.Value.AsString() == "true" {
				return sample(psc)
			}
			if a.Key == "error" && a.Value.Type() == attribute.BOOL && a.Value.AsBool() {
				return sample(psc)
			}
		}
	}

	// 3) 规则匹配
	for _, r := range s.rules {
		if r.compiledRegex != nil && !r.compiledRegex.MatchString(p.Name) {
			continue
		}
		if r.AttrKey != "" {
			matched := false
			for _, a := range p.Attributes {
				if string(a.Key) == r.AttrKey && a.Value.Emit() == r.AttrEquals {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		// 命中 → 用 TraceIDRatioBased 的等效逻辑决定
		return ratioSample(r.Ratio, p, psc)
	}

	// 4) 否则走默认 (parent-based fixed ratio)
	return s.defaultSampler.ShouldSample(p)
}

// Description 实现 sdktrace.Sampler 接口.
func (s *AdaptiveSampler) Description() string {
	return "AdaptiveSampler{rules:" + strconv.Itoa(len(s.rules)) +
		",errorAlwaysSample:" + strconv.FormatBool(s.errorSample) + "}"
}

// sample 总是采样.
func sample(psc trace.SpanContext) sdktrace.SamplingResult {
	return sdktrace.SamplingResult{
		Decision:   sdktrace.RecordAndSample,
		Tracestate: psc.TraceState(),
	}
}

// ratioSample 按 traceID 哈希决定.
func ratioSample(ratio float64, p sdktrace.SamplingParameters, psc trace.SpanContext) sdktrace.SamplingResult {
	if ratio >= 1.0 {
		return sample(psc)
	}
	if ratio <= 0 {
		return sdktrace.SamplingResult{
			Decision:   sdktrace.Drop,
			Tracestate: psc.TraceState(),
		}
	}
	// TraceID 是 16 字节; 取前 8 字节作 uint64,跟阈值比.
	tid := p.TraceID
	threshold := uint64(ratio * float64(1<<63))
	x := uint64(tid[0])<<56 | uint64(tid[1])<<48 | uint64(tid[2])<<40 |
		uint64(tid[3])<<32 | uint64(tid[4])<<24 | uint64(tid[5])<<16 |
		uint64(tid[6])<<8 | uint64(tid[7])
	x &= (1<<63 - 1)
	if x < threshold {
		return sample(psc)
	}
	return sdktrace.SamplingResult{Decision: sdktrace.Drop, Tracestate: psc.TraceState()}
}

// LoadSamplerOptsFromEnv 从 env 读配置.
//
//   - OTEL_SAMPLE_RATIO:       默认 ratio (e.g. 0.05)
//   - OTEL_SAMPLING_RULES_JSON: JSON 数组 of SamplerRule
//   - OTEL_ERROR_ALWAYS_SAMPLE: "true" → 错误 span 100% 采样
func LoadSamplerOptsFromEnv() SamplerOpts {
	out := SamplerOpts{DefaultRatio: 0.05, ErrorAlwaysSample: true}
	if v := os.Getenv("OTEL_SAMPLE_RATIO"); v != "" {
		if r, err := strconv.ParseFloat(v, 64); err == nil {
			out.DefaultRatio = r
		}
	}
	if v := os.Getenv("OTEL_SAMPLING_RULES_JSON"); v != "" {
		var rules []SamplerRule
		if err := json.Unmarshal([]byte(v), &rules); err == nil {
			out.Rules = rules
		}
	}
	if v := os.Getenv("OTEL_ERROR_ALWAYS_SAMPLE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			out.ErrorAlwaysSample = b
		}
	}
	return out
}
