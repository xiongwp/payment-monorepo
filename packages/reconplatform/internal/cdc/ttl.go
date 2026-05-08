// TTL 解析：每条事件存进 Redis 时，按 service + table 决定过期时间。
//
// 配置来源：config-center key=reconplatform/cdc.ttl，JSON 形态（OnChange 热更）：
//
//	{
//	  "default": "30d",                                  // 全局兜底（必填）
//	  "order-core": "7d",                                // 服务级
//	  "order-core/payment_intents": "30d",               // 表级（最优先）
//	  "accounting-system/account_transaction": "90d",
//	  "payment-channel/card_charges": "90d",
//	  "*/audit_log": "365d"                              // 跨服务 audit_log 都长留
//	}
//
// 解析顺序（命中即返）：
//
//	1. "<svc>/<table>"     精确表
//	2. "*/<table>"         任意服务的同名表（如 audit_log）
//	3. "<svc>"             整个服务默认
//	4. "default"           全局
//
// 单位支持："30d"=30天 "12h" "30m" "1h30m" 等 Go time.ParseDuration 能识别 +
// "<N>d" 扩展（time.ParseDuration 不认 'd'，需要手动展开）。
package cdc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// TTLConfig 一份配置快照（不可变；OnChange 用 atomic.Pointer 整体替换）。
type TTLConfig struct {
	raw map[string]string // 原始 string，便于 dump 排查
	// 解析后的 duration（避免 hot path 反复 parse）
	parsed map[string]time.Duration
	// 全局兜底；解析失败则用代码默认 30 天
	defaultTTL time.Duration
}

// NewTTLConfig 解析一份 TTL JSON。空 map 等价于"全用默认 30d"。
// 解析失败的条目跳过 + 写到 invalid 列表（caller 可日志告警）。
func NewTTLConfig(raw map[string]string) (*TTLConfig, []string) {
	c := &TTLConfig{
		raw:        raw,
		parsed:     make(map[string]time.Duration, len(raw)),
		defaultTTL: 30 * 24 * time.Hour,
	}
	var invalid []string
	for k, v := range raw {
		d, err := parseDuration(v)
		if err != nil {
			invalid = append(invalid, fmt.Sprintf("%s=%q: %v", k, v, err))
			continue
		}
		c.parsed[k] = d
		if k == "default" {
			c.defaultTTL = d
		}
	}
	return c, invalid
}

// Resolve 按命中顺序找出 (service, table) 的 TTL。
func (c *TTLConfig) Resolve(service, table string) time.Duration {
	if c == nil {
		return 30 * 24 * time.Hour
	}
	// 1) 精确表
	if d, ok := c.parsed[service+"/"+table]; ok {
		return d
	}
	// 2) 任意服务的同名表
	if d, ok := c.parsed["*/"+table]; ok {
		return d
	}
	// 3) 整个服务
	if d, ok := c.parsed[service]; ok {
		return d
	}
	// 4) 全局兜底
	return c.defaultTTL
}

// Raw 返还 raw JSON 形态（admin web 展示用）。
func (c *TTLConfig) Raw() map[string]string {
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.raw))
	for k, v := range c.raw {
		out[k] = v
	}
	return out
}

// dayPattern 识别 "<N>d" 形式（N 可以是整数或带小数：1.5d）。
// 之所以单写：time.ParseDuration 只认到 "h"，没有 "d"。
var dayPattern = regexp.MustCompile(`^(\d+(?:\.\d+)?)d$`)

// parseDuration 同 time.ParseDuration 但额外支持 "<N>d" = N 天。
// 例：parseDuration("30d") = 720h；parseDuration("1.5d") = 36h
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if m := dayPattern.FindStringSubmatch(s); m != nil {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("parse %q: must be > 0", s)
	}
	return d, nil
}

// TTLProvider 给 Publisher 用的接口；用 atomic.Pointer 让 OnChange 热更
// 不阻塞 hot-path 写入。
type TTLProvider struct {
	cur atomic.Pointer[TTLConfig]
}

// NewTTLProvider 构造一个永远走 default=30d 的 provider；启动期立即 Reload 真实配置。
func NewTTLProvider() *TTLProvider {
	p := &TTLProvider{}
	def, _ := NewTTLConfig(map[string]string{"default": "30d"})
	p.cur.Store(def)
	return p
}

// Reload 替换内存配置。OnChange 回调里调，无锁、热更。
// 返 invalid 列表给 caller 决定是否告警。
func (p *TTLProvider) Reload(raw map[string]string) []string {
	cfg, invalid := NewTTLConfig(raw)
	p.cur.Store(cfg)
	return invalid
}

// Get hot-path：读当前 TTL 配置（原子加载，无锁）。
func (p *TTLProvider) Get() *TTLConfig {
	return p.cur.Load()
}

// Resolve 直接拿 (service, table) 的 TTL，最常用的写法。
func (p *TTLProvider) Resolve(service, table string) time.Duration {
	return p.cur.Load().Resolve(service, table)
}
