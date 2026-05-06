// typed.go：SDK 类型化读取 API。
//
// 业务调用方很少想拿 raw string；多数场景就是「取一个 int 限流」「取一个 bool
// flag」「取一个 string 列表」「取一个嵌套 struct」。本文件提供面向值类型的
// helper，跟 Get() 共用同一份 cache（纳秒级 atomic 读，无 IO）。
//
// 设计纪律：
//
//  1. **永远有 fallback**：每个 GetXxx 都接受 def 参数；key 不存在 / 不在
//     生效窗口 / 解析失败 → 返 def。业务热路径不应处理 error。
//     需要严格区分「不存在」「解析失败」的场景用底层 Get(ctx, key) +
//     自己 strconv / json.Unmarshal。
//
//  2. **format 容错**：plain / json 都尽力解析。
//     - plain "123" → GetInt 返 123
//     - json "123"  → GetInt 也返 123（json 数字字面量）
//     - json `"true"` → GetBool 返 true（json 字符串）
//     - plain "true"  → GetBool 也返 true
//     这样 admin 写 yaml 时不用关心 format 字段，业务读时一致。
//
//  3. **list / map / struct 走 GetJSON[T]**：泛型一把梭，避免每种 list 类型一个
//     accessor。caller 显式给类型，类型不匹配返 def。
//
//  4. **零分配热路径**：GetString / GetInt 等基本类型不分配；GetJSON 必然
//     分配（unmarshal 目标）。高频调用建议配合 Bind[T] 一次性绑到
//     atomic.Pointer[T]，业务侧 .Load() 读，不再走 GetJSON。
//
// 用法：
//
//	rps   := cli.GetInt(ctx, "rate_limit.rps", 100)
//	flag  := cli.GetBool(ctx, "feature.new_router", false)
//	hosts := cli.GetStringList(ctx, "kafka.brokers", []string{"localhost:9092"})
//
//	type Routing struct {
//	    Primary   string   `json:"primary"`
//	    Fallback  []string `json:"fallback"`
//	    TimeoutMs int      `json:"timeout_ms"`
//	}
//	var r Routing
//	if err := configcenter.GetJSON(cli, ctx, "card.routing", &r); err != nil {
//	    r = Routing{Primary: "stripe", TimeoutMs: 3000}
//	}
package configcenter

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// GetInt 取整型；失败返 def。支持 plain / json 两种 format。
//
// 解析顺序：
//  1. plain：strconv.Atoi 直接试
//  2. json：json.Unmarshal 进 int（兼容 "123" 字符串字面量 fallback strconv）
func (c *Client) GetInt(ctx context.Context, key string, def int) int {
	v, err := c.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	s := strings.TrimSpace(v.Value)
	if s == "" {
		return def
	}
	// 1) plain 尝试
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	// 2) json 数字字面量（含 "123" 这种 quoted 写法）
	var n int
	if err := json.Unmarshal([]byte(s), &n); err == nil {
		return n
	}
	var qs string
	if err := json.Unmarshal([]byte(s), &qs); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(qs)); err == nil {
			return n
		}
	}
	return def
}

// GetInt64 同 GetInt，64 位。
func (c *Client) GetInt64(ctx context.Context, key string, def int64) int64 {
	v, err := c.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	s := strings.TrimSpace(v.Value)
	if s == "" {
		return def
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	var n int64
	if err := json.Unmarshal([]byte(s), &n); err == nil {
		return n
	}
	return def
}

// GetFloat64 取浮点；失败返 def。
func (c *Client) GetFloat64(ctx context.Context, key string, def float64) float64 {
	v, err := c.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	s := strings.TrimSpace(v.Value)
	if s == "" {
		return def
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	var f float64
	if err := json.Unmarshal([]byte(s), &f); err == nil {
		return f
	}
	return def
}

// GetBool 取布尔；失败返 def。接受 true/false/1/0/yes/no/on/off (大小写不敏感)
// 以及 json bool 字面量。
func (c *Client) GetBool(ctx context.Context, key string, def bool) bool {
	v, err := c.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	s := strings.TrimSpace(strings.ToLower(v.Value))
	switch s {
	case "true", "1", "yes", "on", "y", "t":
		return true
	case "false", "0", "no", "off", "n", "f":
		return false
	}
	// json bool
	var b bool
	if err := json.Unmarshal([]byte(v.Value), &b); err == nil {
		return b
	}
	return def
}

// GetDuration 取 time.Duration（接受 "30s" / "5m" / "2h30m"）。失败返 def。
//
// 不支持纯数字（避免单位歧义）— "30" 不返 30s，会返 def。让 admin 必须写
// 完整 "30s"，避免 prod 配错把 ms 当 s 写。
func (c *Client) GetDuration(ctx context.Context, key string, def time.Duration) time.Duration {
	v, err := c.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	s := strings.TrimSpace(v.Value)
	if s == "" {
		return def
	}
	// 兼容 quoted "30s"
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var qs string
		if err := json.Unmarshal([]byte(s), &qs); err == nil {
			s = qs
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return def
}

// GetStringList 取字符串数组。支持：
//   - json：`["a","b","c"]`
//   - plain CSV：`a,b,c`
//   - plain 单值："stripe" → ["stripe"]
//
// 失败 / 空 → def。
func (c *Client) GetStringList(ctx context.Context, key string, def []string) []string {
	v, err := c.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	s := strings.TrimSpace(v.Value)
	if s == "" {
		return def
	}
	// 1) JSON 数组
	if strings.HasPrefix(s, "[") {
		var out []string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	}
	// 2) CSV
	if strings.Contains(s, ",") {
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
		return def
	}
	// 3) 单值
	return []string{s}
}

// GetIntList 取整型数组。同 GetStringList 接受 json `[1,2,3]` / CSV `"1,2,3"`。
func (c *Client) GetIntList(ctx context.Context, key string, def []int) []int {
	v, err := c.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	s := strings.TrimSpace(v.Value)
	if s == "" {
		return def
	}
	if strings.HasPrefix(s, "[") {
		var out []int
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	}
	if strings.Contains(s, ",") {
		parts := strings.Split(s, ",")
		out := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return def
			}
			out = append(out, n)
		}
		return out
	}
	if n, err := strconv.Atoi(s); err == nil {
		return []int{n}
	}
	return def
}

// GetMap 取 string → string 字典（json）。失败返 def。
//
// 业务复杂 map（嵌套 / 非 string value）走 GetJSON 自带泛型。
func (c *Client) GetMap(ctx context.Context, key string, def map[string]string) map[string]string {
	v, err := c.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	s := strings.TrimSpace(v.Value)
	if s == "" {
		return def
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return def
	}
	return out
}

// ─── 复杂类型（泛型） ─────────────────────────────────────────────────────

// GetJSON 把 key 的 value 反序列化到 dst（必须是指针）。常用于嵌套 struct /
// map[string]any / 复杂 list。
//
// 用法：
//
//	type Routing struct {
//	    Primary   string   `json:"primary"`
//	    Fallback  []string `json:"fallback"`
//	    TimeoutMs int      `json:"timeout_ms"`
//	}
//	var r Routing
//	if err := configcenter.GetJSON(cli, ctx, "card.routing", &r); err != nil {
//	    r = Routing{Primary: "stripe"} // fallback
//	}
//
// 注意：本函数每次调用都会 unmarshal（分配）。高频热路径用 Bind[T] 一次性绑
// atomic.Pointer[T]，业务侧 .Load() 读，零分配。
//
// 错误：
//   - ErrNotFound  / ErrNotEffective：key 缺失或不在窗口
//   - 解析错        ：json.Unmarshal 的原始 err 包一层返
func GetJSON[T any](c *Client, ctx context.Context, key string, dst *T) error {
	if c == nil || dst == nil {
		return errors.New("configcenter.GetJSON: nil client / dst")
	}
	v, err := c.Get(ctx, key)
	if err != nil {
		return err
	}
	if v == nil || v.Value == "" {
		return ErrNotFound
	}
	if err := json.Unmarshal([]byte(v.Value), dst); err != nil {
		return &TypedDecodeError{Key: key, Format: v.Format, Cause: err}
	}
	return nil
}

// MustGetJSON 同 GetJSON 但失败时把 dst 设为 fallback。返 true 表示从 cache 读
// 成功，false 表示用了 fallback。
//
// 用法：
//
//	r := Routing{Primary: "stripe"}
//	configcenter.MustGetJSON(cli, ctx, "card.routing", &r)
func MustGetJSON[T any](c *Client, ctx context.Context, key string, dst *T) bool {
	if err := GetJSON(c, ctx, key, dst); err != nil {
		return false
	}
	return true
}

// TypedDecodeError 解析失败的明细，包 caller 调试。
type TypedDecodeError struct {
	Key    string
	Format string
	Cause  error
}

func (e *TypedDecodeError) Error() string {
	return "configcenter: decode key=" + e.Key + " format=" + e.Format + ": " + e.Cause.Error()
}

func (e *TypedDecodeError) Unwrap() error { return e.Cause }
