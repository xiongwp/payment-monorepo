// source.go — Source 接口的 2 个实现: memory (测试) + configCenter (生产)。

package featureflag

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// MemorySource 内存 source — 单测 / dev。
type MemorySource struct {
	mu    sync.RWMutex
	flags map[string]Flag
}

// NewMemorySource 构造。
func NewMemorySource(flags map[string]Flag) *MemorySource {
	if flags == nil {
		flags = map[string]Flag{}
	}
	cp := make(map[string]Flag, len(flags))
	for k, v := range flags {
		cp[k] = v
	}
	return &MemorySource{flags: cp}
}

// Load impl。
func (m *MemorySource) Load() (map[string]Flag, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Flag, len(m.flags))
	for k, v := range m.flags {
		out[k] = v
	}
	return out, nil
}

// Set 设一个 flag (单测里随时改)。
func (m *MemorySource) Set(name string, f Flag) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f.Name == "" {
		f.Name = name
	}
	m.flags[name] = f
}

// ─── config-center source ──────────────────────────────────────────────

// ConfigCenterClient 抽象 — 跟现有 payment-util/configcenter 解耦。
//   GetJSON(ctx, key, out): 拉一段 JSON 反序列化。
//   实现见 payment-util/configcenter/client.go。
type ConfigCenterClient interface {
	GetJSON(ctx context.Context, key string, out any) error
}

// ConfigCenterSource 从 config-center 的 "featureflags" key 拉 [{name, ...}, ...] 列表。
type ConfigCenterSource struct {
	cc  ConfigCenterClient
	key string // 默认 "featureflags"
}

// NewConfigCenterSource 构造。
func NewConfigCenterSource(cc ConfigCenterClient, key string) *ConfigCenterSource {
	if key == "" {
		key = "featureflags"
	}
	return &ConfigCenterSource{cc: cc, key: key}
}

// Load impl。
//
// config-center 里 "featureflags" 存的格式:
//   {
//     "flags": [
//       {"name":"new_refund_engine", "type":"bool", "default":false,
//        "rollout_percent":10, "include_keys":["mer_001"]},
//       {"name":"payment.refund.kill_switch", "type":"bool", "default":false}
//     ]
//   }
func (s *ConfigCenterSource) Load() (map[string]Flag, error) {
	if s.cc == nil {
		return nil, errors.New("config-center client nil")
	}
	var doc struct {
		Flags []Flag `json:"flags"`
	}
	if err := s.cc.GetJSON(context.Background(), s.key, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]Flag, len(doc.Flags))
	for _, f := range doc.Flags {
		if f.Name != "" {
			out[f.Name] = f
		}
	}
	return out, nil
}

// JSONLoader 单文件加载器 — 兜底, dev 用 yaml/json 文件。
func JSONLoader(raw []byte) (map[string]Flag, error) {
	var doc struct {
		Flags []Flag `json:"flags"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]Flag, len(doc.Flags))
	for _, f := range doc.Flags {
		if f.Name != "" {
			out[f.Name] = f
		}
	}
	return out, nil
}
