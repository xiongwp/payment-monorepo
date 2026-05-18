// readthrough.go — typed read-through + singleflight helpers.
//
// Singleflight 防雪崩:
//   - 同一 key 在 cache miss 时如果有 N 个 goroutine 并发访问,只让其中 1 个
//     真正调 loader; 其余等结果. 防止"缓存击穿"打挂下游 DB / 远程服务.
//   - 实现走标准库风格的 mutex + map, 不依赖 x/sync/singleflight 包 (减少 dep).

package cachelib

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Codec 把领域对象 ⇄ []byte. 默认 JSON.
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

// JSONCodec 默认 JSON 编解码.
type JSONCodec struct{}

// Marshal …
func (JSONCodec) Marshal(v any) ([]byte, error)        { return json.Marshal(v) }

// Unmarshal …
func (JSONCodec) Unmarshal(data []byte, v any) error   { return json.Unmarshal(data, v) }

// ReadThroughOptions 控制 ReadThrough 行为.
type ReadThroughOptions struct {
	// TTL 命中后缓存的过期时间; <=0 用 Cache 实现默认 TTL.
	TTL time.Duration
	// Codec 业务对象序列化方式, 默认 JSONCodec.
	Codec Codec
	// CacheNotFound true → loader 返 ErrNotFound 时也缓存 (空值/负缓存),
	//   防止热点 key 不存在打 DB. 缓存值为空 byte slice + 短 TTL (TTL/10).
	CacheNotFound bool
}

// 进程内 singleflight (per Cache 实例独立).
//
// 简化版: cache miss 时拿一把锁查 inflight; 没有则自己 load, 完成后通知;
// 已有则等 notify channel.
type singleflightGroup struct {
	mu       sync.Mutex
	inflight map[string]*sfCall
}

type sfCall struct {
	wg  sync.WaitGroup
	val []byte
	err error
}

func newSFGroup() *singleflightGroup {
	return &singleflightGroup{inflight: make(map[string]*sfCall)}
}

func (g *singleflightGroup) do(key string, fn func() ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	if c, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &sfCall{}
	c.wg.Add(1)
	g.inflight[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.inflight, key)
	g.mu.Unlock()
	return c.val, c.err
}

// 全局 singleflight group; 简化为 process-global. 实际并发量大时每个 Cache 实例
// 独立 group 更精准, 但当前 N(key 集合) 远 > N(并发线程), 全局够用.
var sfDefault = newSFGroup()

// ReadThrough 标准读穿透: 先查 cache, miss 时调 loader 并回填.
//
// 返回:
//   - 命中 / load 成功: 反序列化后的 v.
//   - loader 返 ErrNotFound 且 opts.CacheNotFound=false: 透传 ErrNotFound.
//   - loader 返其它错: 透传原错; 不缓存.
//
// 注: cache.Get / Set 失败不会阻塞业务 — log + degrade 到直接调 loader.
// 这样 Redis 抖动不会让用户请求失败.
func ReadThrough[T any](
	ctx context.Context,
	cache Cache,
	key string,
	loader func(ctx context.Context) (T, error),
	opts ReadThroughOptions,
) (T, error) {
	var zero T
	codec := opts.Codec
	if codec == nil {
		codec = JSONCodec{}
	}
	// 1. 查 cache
	if cache != nil {
		raw, ok, err := cache.Get(ctx, key)
		if err == nil && ok {
			if len(raw) == 0 && opts.CacheNotFound {
				return zero, ErrNotFound
			}
			var out T
			if uerr := codec.Unmarshal(raw, &out); uerr == nil {
				return out, nil
			}
			// 解码失败 — 老 schema 残留, fallthrough 重新 load
		}
	}
	// 2. singleflight load
	raw, lerr := sfDefault.do(key, func() ([]byte, error) {
		v, err := loader(ctx)
		if err != nil {
			if err == ErrNotFound && opts.CacheNotFound && cache != nil {
				// 缓存空值, 短 TTL
				ttl := opts.TTL / 10
				if ttl < time.Second {
					ttl = time.Second
				}
				_ = cache.Set(ctx, key, nil, ttl)
			}
			return nil, err
		}
		b, merr := codec.Marshal(v)
		if merr != nil {
			return nil, merr
		}
		if cache != nil {
			_ = cache.Set(ctx, key, b, opts.TTL)
		}
		return b, nil
	})
	if lerr != nil {
		return zero, lerr
	}
	var out T
	if uerr := codec.Unmarshal(raw, &out); uerr != nil {
		return zero, uerr
	}
	return out, nil
}

// Invalidate 删除指定 key (写路径调用, e.g. UpdateStatus 后).
//
// Convenience — 等价 cache.Del 但允许后续接入 metrics / 异步 publish 等.
func Invalidate(ctx context.Context, cache Cache, keys ...string) error {
	if cache == nil || len(keys) == 0 {
		return nil
	}
	return cache.Del(ctx, keys...)
}
