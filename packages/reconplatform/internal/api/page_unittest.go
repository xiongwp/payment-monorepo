// page_test.go — UX-1: 规则单元测试框架.
//
// 端点:
//   POST /api/v1/scripts/_test
//
// Body:
//   {
//     "code": "<starlark source>",
//     "fixtures": [ store.Event, ... ],   // 喂给 FixtureSearcher
//     "expected": [ script.Diff, ... ],    // 期望的 diff
//     "params":   { "..." : "..." },       // 可选
//     "match":    "subset" | "exact"       // 默认 subset
//   }
//
// 返:
//   {
//     "passed":  bool,
//     "actual":  [ script.Diff, ... ],
//     "missing": [ expected diff that didn't appear ],
//     "extra":   [ actual diff not in expected (exact mode only) ],
//     "exec_ms": int,
//     "error":   "<if script err>",
//   }
//
// 比较语义:
//   - subset (默认):每条 expected diff 必须有 actual 命中 (type+key 相等;
//     若 expected 写了 want/got/detail 字段,subset 匹配 actual 的对应字段).
//   - exact: 1-1 mapping, 不允许多 / 少.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
)

type testRequest struct {
	Code     string            `json:"code"`
	Fixtures []store.Event     `json:"fixtures"`
	Expected []script.Diff     `json:"expected"`
	Params   map[string]string `json:"params"`
	Match    string            `json:"match"` // "subset" / "exact"
}

type testResponse struct {
	Passed  bool          `json:"passed"`
	Actual  []script.Diff `json:"actual"`
	Missing []script.Diff `json:"missing"`
	Extra   []script.Diff `json:"extra"`
	ExecMS  int64         `json:"exec_ms"`
	Error   string        `json:"error,omitempty"`
}

// scriptTest POST /api/v1/scripts/_test.
func (s *Server) scriptTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req testRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("empty code"))
		return
	}
	if req.Match == "" {
		req.Match = "subset"
	}

	// FixtureSearcher 替代真实 Redis 数据源 → 测试 hermetic.
	fs := store.NewFixtureSearcher(req.Fixtures)

	runCtx, cancel := context.WithTimeout(r.Context(), 1*time.Minute)
	defer cancel()
	if req.Params == nil {
		req.Params = map[string]string{}
	}
	req.Params["test"] = "true"

	sctx := script.NewContext(runCtx, fs, s.zapAdapter(), req.Params)
	started := time.Now()
	res := s.loader.RunCode(sctx, "_test", req.Code, "test:"+actorOrUnknown(r))
	elapsed := time.Since(started).Milliseconds()

	resp := testResponse{
		Actual: res.Diffs,
		ExecMS: elapsed,
		Error:  res.Error,
	}
	if res.Status != "success" {
		// 编译错 / runtime 错 → fail
		resp.Passed = false
		resp.Missing = req.Expected
		writeJSON(w, http.StatusOK, resp)
		return
	}

	missing, extra := compareDiffs(req.Expected, res.Diffs, req.Match)
	resp.Missing = missing
	resp.Extra = extra
	resp.Passed = len(missing) == 0 && (req.Match != "exact" || len(extra) == 0)
	writeJSON(w, http.StatusOK, resp)
}

// compareDiffs 把 expected 与 actual 配对.
//
// subset 模式: 每个 expected 找一条匹配的 actual (type/key 等, 字段子集等).
// exact 模式: 同时算出 actual 中没被 expected 覆盖的 extra.
//
// 一条 actual 最多匹配一条 expected (避免一个 actual 同时满足多个 expected 的歧义).
func compareDiffs(expected, actual []script.Diff, mode string) (missing, extra []script.Diff) {
	used := make([]bool, len(actual))
	for _, e := range expected {
		matched := -1
		for i, a := range actual {
			if used[i] {
				continue
			}
			if diffMatches(e, a) {
				matched = i
				break
			}
		}
		if matched < 0 {
			missing = append(missing, e)
		} else {
			used[matched] = true
		}
	}
	if mode == "exact" {
		for i, a := range actual {
			if !used[i] {
				extra = append(extra, a)
			}
		}
	}
	return missing, extra
}

// diffMatches expected ⊆ actual (字段级 subset).
//
// 必匹配字段:
//   - Type 不空: 必须相等
//   - Key 不空:  必须相等
//
// 可选匹配:
//   - Want / Got 非 nil: 必须 reflect.DeepEqual
//   - Detail 非 nil 且为 map: subset 检查 (期望的 keys 都在 actual.Detail 中且值相等)
//                          其它类型: reflect.DeepEqual
func diffMatches(expected, actual script.Diff) bool {
	if expected.Type != "" && expected.Type != actual.Type {
		return false
	}
	if expected.Key != "" && expected.Key != actual.Key {
		return false
	}
	if expected.Want != nil && !reflect.DeepEqual(expected.Want, actual.Want) {
		return false
	}
	if expected.Got != nil && !reflect.DeepEqual(expected.Got, actual.Got) {
		return false
	}
	if expected.Detail != nil {
		if !subsetEqual(expected.Detail, actual.Detail) {
			return false
		}
	}
	return true
}

// subsetEqual 检查 want 是否是 got 的字段子集 (map case) 或 deepEqual.
func subsetEqual(want, got any) bool {
	wm, wok := want.(map[string]any)
	gm, gok := got.(map[string]any)
	if wok && gok {
		for k, v := range wm {
			if av, ok := gm[k]; !ok || !reflect.DeepEqual(v, av) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(want, got)
}
