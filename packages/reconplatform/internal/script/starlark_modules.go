// starlark_modules.go — Starlark 内置模块注册。
//
// 提供 4 类模块：
//
//  1. **stdlib 等价**：json / time / math / strings / regex
//     来自 starlark 官方 lib (go.starlark.net/lib/...) 或本地手写 wrapper
//
//  2. **recon 业务模块**：暴露 ctx-相关 helper（虽然 ctx 已经是 callable，但
//     有些工具函数（如 last_n_hours）适合放包级别）
//
//  3. **engine.RegisterModule 注入的 host 包**（运行时动态加，不重启）
//
//  4. **load("helper.star", ...)** 脚本互相 load — 暂未实现，留 follow-up
//
// 脚本作者用法：
//
//	load("@json", "encode", "decode")
//	load("@time", "now", "parse_time")
//	load("@strings", "split", "to_upper")
//	load("@recon", "last_n_hours")
//
//	def check(ctx):
//	    payload = json.encode({"hello": "world"})
//	    parts = strings.split("a,b,c", ",")
//	    since, until = last_n_hours(ctx.now, 24)
//	    ...

package script

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	starlarkjson "go.starlark.net/lib/json"
	starlarkmath "go.starlark.net/lib/math"
	starlarktime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
)

// registerBuiltinModules 把开箱即用的模块塞进 engine.modules。
//
// 模块名（脚本里 load("@<name>", ...) 用）：json / time / math / strings / regex / recon
func registerBuiltinModules(e *Engine) {
	// json (官方)
	e.RegisterModule("json", starlark.StringDict{
		"encode": starlarkjson.Module.Members["encode"],
		"decode": starlarkjson.Module.Members["decode"],
		"indent": starlarkjson.Module.Members["indent"],
	})
	// time (官方)
	e.RegisterModule("time", starlark.StringDict{
		"now":              starlarktime.Module.Members["now"],
		"from_timestamp":   starlarktime.Module.Members["from_timestamp"],
		"parse_time":       starlarktime.Module.Members["parse_time"],
		"parse_duration":   starlarktime.Module.Members["parse_duration"],
		"time":             starlarktime.Module.Members["time"],
		"hour":             starlarktime.Module.Members["hour"],
		"minute":           starlarktime.Module.Members["minute"],
		"second":           starlarktime.Module.Members["second"],
		"millisecond":      starlarktime.Module.Members["millisecond"],
	})
	// math (官方)
	e.RegisterModule("math", starlark.StringDict{
		"abs":  starlarkmath.Module.Members["fabs"],
		"ceil": starlarkmath.Module.Members["ceil"],
		"floor": starlarkmath.Module.Members["floor"],
		"sqrt": starlarkmath.Module.Members["sqrt"],
		"pow":  starlarkmath.Module.Members["pow"],
		"log":  starlarkmath.Module.Members["log"],
	})
	// strings — 自己包，starlark 官方没有
	e.RegisterModule("strings", strSubModule())
	// regex — 自己包
	e.RegisterModule("regex", regexSubModule())
	// recon — 业务 helper
	e.RegisterModule("recon", reconSubModule())
}

// strSubModule 提供 split / join / to_lower / to_upper / contains / has_prefix / has_suffix / trim。
func strSubModule() starlark.StringDict {
	return starlark.StringDict{
		"split": starlark.NewBuiltin("strings.split", strSplit),
		"join":  starlark.NewBuiltin("strings.join", strJoin),
		"to_lower":   starlark.NewBuiltin("strings.to_lower", strToLower),
		"to_upper":   starlark.NewBuiltin("strings.to_upper", strToUpper),
		"contains":   starlark.NewBuiltin("strings.contains", strContains),
		"has_prefix": starlark.NewBuiltin("strings.has_prefix", strHasPrefix),
		"has_suffix": starlark.NewBuiltin("strings.has_suffix", strHasSuffix),
		"trim":       starlark.NewBuiltin("strings.trim", strTrim),
		"replace":    starlark.NewBuiltin("strings.replace", strReplace),
	}
}

func strSplit(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var s, sep string
	if err := starlark.UnpackArgs("split", args, nil, "s", &s, "sep", &sep); err != nil {
		return nil, err
	}
	parts := strings.Split(s, sep)
	out := starlark.NewList(make([]starlark.Value, 0, len(parts)))
	for _, p := range parts {
		_ = out.Append(starlark.String(p))
	}
	return out, nil
}

func strJoin(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var list *starlark.List
	var sep string
	if err := starlark.UnpackArgs("join", args, nil, "list", &list, "sep", &sep); err != nil {
		return nil, err
	}
	parts := make([]string, 0, list.Len())
	iter := list.Iterate()
	defer iter.Done()
	var v starlark.Value
	for iter.Next(&v) {
		s, ok := v.(starlark.String)
		if !ok {
			return nil, fmt.Errorf("strings.join: expected string list element, got %s", v.Type())
		}
		parts = append(parts, string(s))
	}
	return starlark.String(strings.Join(parts, sep)), nil
}

func strToLower(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs("to_lower", args, nil, "s", &s); err != nil {
		return nil, err
	}
	return starlark.String(strings.ToLower(s)), nil
}

func strToUpper(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs("to_upper", args, nil, "s", &s); err != nil {
		return nil, err
	}
	return starlark.String(strings.ToUpper(s)), nil
}

func strContains(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var s, sub string
	if err := starlark.UnpackArgs("contains", args, nil, "s", &s, "sub", &sub); err != nil {
		return nil, err
	}
	return starlark.Bool(strings.Contains(s, sub)), nil
}

func strHasPrefix(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var s, prefix string
	if err := starlark.UnpackArgs("has_prefix", args, nil, "s", &s, "prefix", &prefix); err != nil {
		return nil, err
	}
	return starlark.Bool(strings.HasPrefix(s, prefix)), nil
}

func strHasSuffix(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var s, suffix string
	if err := starlark.UnpackArgs("has_suffix", args, nil, "s", &s, "suffix", &suffix); err != nil {
		return nil, err
	}
	return starlark.Bool(strings.HasSuffix(s, suffix)), nil
}

func strTrim(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var s, cut string
	cut = " \t\n\r"
	if err := starlark.UnpackArgs("trim", args, nil, "s", &s, "cut?", &cut); err != nil {
		return nil, err
	}
	return starlark.String(strings.Trim(s, cut)), nil
}

func strReplace(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var s, old, new string
	n := -1
	if err := starlark.UnpackArgs("replace", args, nil, "s", &s, "old", &old, "new", &new, "n?", &n); err != nil {
		return nil, err
	}
	return starlark.String(strings.Replace(s, old, new, n)), nil
}

// regexSubModule: regex.match(pattern, s) / regex.find_all(pattern, s)
func regexSubModule() starlark.StringDict {
	return starlark.StringDict{
		"match":    starlark.NewBuiltin("regex.match", regexMatch),
		"find_all": starlark.NewBuiltin("regex.find_all", regexFindAll),
	}
}

func regexMatch(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var pattern, s string
	if err := starlark.UnpackArgs("match", args, nil, "pattern", &pattern, "s", &s); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("regex.match: invalid pattern: %w", err)
	}
	return starlark.Bool(re.MatchString(s)), nil
}

func regexFindAll(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var pattern, s string
	limit := -1
	if err := starlark.UnpackArgs("find_all", args, nil, "pattern", &pattern, "s", &s, "limit?", &limit); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("regex.find_all: invalid pattern: %w", err)
	}
	hits := re.FindAllString(s, limit)
	out := starlark.NewList(make([]starlark.Value, 0, len(hits)))
	for _, h := range hits {
		_ = out.Append(starlark.String(h))
	}
	return out, nil
}

// reconSubModule: 业务 helper（last_n_hours / today / format_money / hash_pii 等）
func reconSubModule() starlark.StringDict {
	return starlark.StringDict{
		"last_n_hours": starlark.NewBuiltin("recon.last_n_hours", reconLastNHours),
		"http_get":     starlark.NewBuiltin("recon.http_get", reconHTTPGet),
	}
}

// reconLastNHours(now, n) → (since, until) 字符串对（RFC3339）
func reconLastNHours(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var nowStr string
	var n int
	if err := starlark.UnpackArgs("last_n_hours", args, nil, "now", &nowStr, "n", &n); err != nil {
		return nil, err
	}
	now, err := time.Parse(time.RFC3339, nowStr)
	if err != nil {
		return nil, fmt.Errorf("last_n_hours: now must be RFC3339 string: %w", err)
	}
	since := now.Add(time.Duration(-n) * time.Hour)
	return starlark.Tuple{
		starlark.String(since.Format(time.RFC3339)),
		starlark.String(now.Format(time.RFC3339)),
	}, nil
}

// reconHTTPGet(url, timeout_sec=5) → dict {"status": int, "body": str}
//
// 受限 HTTP client：
//   - 仅 GET，无 body
//   - 默认 timeout 5s（防慢端点拖死脚本）
//   - 脚本无法注入 header（避免泄漏 host 凭据）
//   - 走 thread-local 的 Go ctx，到期立刻 cancel
func reconHTTPGet(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	var url string
	timeoutSec := 5
	if err := starlark.UnpackArgs("http_get", args, nil, "url", &url, "timeout_sec?", &timeoutSec); err != nil {
		return nil, err
	}
	if timeoutSec <= 0 || timeoutSec > 30 {
		timeoutSec = 5
	}
	parent, _ := thread.Local(threadLocalGoCtx).(context.Context)
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("http_get: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_get: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB cap
	d := starlark.NewDict(2)
	_ = d.SetKey(starlark.String("status"), starlark.MakeInt(resp.StatusCode))
	_ = d.SetKey(starlark.String("body"), starlark.String(string(body)))
	if sctx, _ := thread.Local(threadLocalCtx).(*Context); sctx != nil {
		sctx.stats.HTTPCalls++
	}
	return d, nil
}
