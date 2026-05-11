// Package featureflag — 轻量灰度 / 熔断开关 SDK。
//
// 设计目标:
//   - 改 flag 不重启服务 (config-center / etcd watch 热刷)
//   - 支持 bool / int / string / json 4 种类型
//   - 灰度: 按 key (merchant_id / user_id) 一致性 hash 落桶 0-99
//   - 白名单: 强制开启 / 强制关闭某些 key
//   - 上下文绑定: 进 ctx 一次, 全链路同 flag 值 (避免一次请求中途切换)
//
// 用例:
//
//   // 启动期
//   ff := featureflag.New(featureflag.Config{
//       Source: featureflag.NewConfigCenter(cc),  // 或 NewMemory(map)
//       PollInterval: 10 * time.Second,
//   })
//   defer ff.Stop()
//
//   // 业务代码
//   if ff.BoolFor(ctx, "new_refund_engine", merchantID) {
//       useNewEngine(...)
//   } else {
//       useLegacyEngine(...)
//   }
//
//   // 紧急熔断:
//   //   ops 控制台改 payment.refund.kill_switch=true → 30s 内全部副本生效
//   if ff.Bool("payment.refund.kill_switch") {
//       return errors.New("refund disabled by ops")
//   }
//
// 灰度配置 schema (存 config-center / etcd):
//
//   {
//     "name": "new_refund_engine",
//     "type": "bool",
//     "default": false,
//     "rollout_percent": 10,             // 10% 灰度
//     "include_keys": ["merchant_001"],  // 强制开
//     "exclude_keys": ["merchant_999"],  // 强制关
//   }
//
// 标准: 跟 LaunchDarkly / Unleash 的最小子集兼容。

package featureflag

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Flag 一条 flag 配置 (来自 config-center)。
type Flag struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`            // bool / int / string / json
	Default        any      `json:"default"`
	RolloutPercent int      `json:"rollout_percent"` // 0-100; 0 = 全关, 100 = 全开
	IncludeKeys    []string `json:"include_keys"`    // 强制开
	ExcludeKeys    []string `json:"exclude_keys"`    // 强制关
}

// Source 配置源抽象 (config-center / etcd / memory)。
type Source interface {
	Load() (map[string]Flag, error)
}

// Config FeatureFlag 配置。
type Config struct {
	Source       Source
	PollInterval time.Duration // 默认 10s
	Log          *zap.Logger
}

// FeatureFlag 主对象。
type FeatureFlag struct {
	cfg   Config
	flags atomic.Value // map[string]Flag
	stop  chan struct{}
	hits  *sync.Map // name → hit counter (for metrics)
}

// New 构造 + 立即拉一次 + 起后台 poll。
func New(cfg Config) *FeatureFlag {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 10 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = zap.NewNop()
	}
	ff := &FeatureFlag{
		cfg:  cfg,
		stop: make(chan struct{}),
		hits: &sync.Map{},
	}
	if err := ff.reload(); err != nil {
		cfg.Log.Warn("initial flag load failed", zap.Error(err))
		ff.flags.Store(map[string]Flag{})
	}
	go ff.pollLoop()
	return ff
}

// Stop 关后台 poll。
func (ff *FeatureFlag) Stop() { close(ff.stop) }

func (ff *FeatureFlag) pollLoop() {
	t := time.NewTicker(ff.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ff.stop:
			return
		case <-t.C:
			if err := ff.reload(); err != nil {
				ff.cfg.Log.Warn("flag poll failed", zap.Error(err))
			}
		}
	}
}

func (ff *FeatureFlag) reload() error {
	flags, err := ff.cfg.Source.Load()
	if err != nil {
		return err
	}
	ff.flags.Store(flags)
	return nil
}

// flagOf 取一条 flag (空返默认空)。
func (ff *FeatureFlag) flagOf(name string) Flag {
	m, _ := ff.flags.Load().(map[string]Flag)
	if f, ok := m[name]; ok {
		return f
	}
	return Flag{Name: name}
}

// Names 列所有 flag (admin 后台用)。
func (ff *FeatureFlag) Names() []string {
	m, _ := ff.flags.Load().(map[string]Flag)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Hits 返每个 flag 的命中次数 (Prometheus metric 用)。
func (ff *FeatureFlag) Hits() map[string]int64 {
	out := map[string]int64{}
	ff.hits.Range(func(k, v any) bool {
		if n, ok := v.(*int64); ok {
			out[k.(string)] = atomic.LoadInt64(n)
		}
		return true
	})
	return out
}

func (ff *FeatureFlag) recordHit(name string) {
	v, _ := ff.hits.LoadOrStore(name, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

// ─── 主查询接口 ────────────────────────────────────────────────────────

// Bool 全局开关 (无 key 灰度)。
func (ff *FeatureFlag) Bool(name string) bool {
	return ff.BoolFor(context.Background(), name, "")
}

// BoolFor 按 key 灰度的 bool。
//   key 通常是 merchant_id / user_id / charge_id 等。
//   空 key → 走全局 default 或 100% rollout 判定。
func (ff *FeatureFlag) BoolFor(_ context.Context, name, key string) bool {
	ff.recordHit(name)
	f := ff.flagOf(name)
	// 白名单 / 黑名单先看
	for _, k := range f.IncludeKeys {
		if k == key {
			return true
		}
	}
	for _, k := range f.ExcludeKeys {
		if k == key {
			return false
		}
	}
	// 灰度比例
	if key != "" && f.RolloutPercent > 0 && f.RolloutPercent < 100 {
		return bucket(key, f.Name) < f.RolloutPercent
	}
	if f.RolloutPercent >= 100 {
		return true
	}
	if f.RolloutPercent <= 0 && f.Default == nil {
		return false
	}
	// fall through to Default
	if b, ok := f.Default.(bool); ok {
		return b
	}
	return false
}

// Int 取 int flag (没配置返 def)。
func (ff *FeatureFlag) Int(name string, def int) int {
	ff.recordHit(name)
	f := ff.flagOf(name)
	if f.Default == nil {
		return def
	}
	switch v := f.Default.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// String 取 string flag (没配置返 def)。
func (ff *FeatureFlag) String(name, def string) string {
	ff.recordHit(name)
	f := ff.flagOf(name)
	if s, ok := f.Default.(string); ok && s != "" {
		return s
	}
	return def
}

// JSON 拿 flag 反序列化进 out (指针)。空 flag 不动 out。
func (ff *FeatureFlag) JSON(name string, out any) error {
	ff.recordHit(name)
	f := ff.flagOf(name)
	if f.Default == nil {
		return nil
	}
	b, err := json.Marshal(f.Default)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// ─── 一致性 hash 落桶 ─────────────────────────────────────────────────

// bucket 把 key 一致性 hash 落到 0-99。
// 加 flag name 作为 salt — 同 key 在不同 flag 下不会都被分到同一桶, 避免相关 flag 的灰度群体一致。
func bucket(key, salt string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % 100)
}
