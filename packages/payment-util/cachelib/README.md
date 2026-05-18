# cachelib

Service-shared cache abstraction with pluggable backends.

## Why

Before: each service had its own `sync.Map` + TTL impl, no cross-replica
visibility, repeated cache stampede patches per service.

Now: one interface + memory / Redis / Noop backends + read-through with
singleflight, reusable across payment-channel / order-core / card-center.

## Quick start

```go
import "github.com/xiongwp/payment-util/cachelib"

// 1. dev / 单测: in-memory.
cache := cachelib.NewMemory(5 * time.Minute)

// 2. prod: redis. Inject any go-redis client wrapped to satisfy RedisClient.
type myRedis struct{ c *redis.Client }
func (a myRedis) Get(ctx context.Context, k string) (string, error) {
    v, err := a.c.Get(ctx, k).Result()
    if errors.Is(err, redis.Nil) { return "", cachelib.ErrNotFound }
    return v, err
}
func (a myRedis) Set(ctx, k, v string, ttl time.Duration) error {
    return a.c.Set(ctx, k, v, ttl).Err()
}
func (a myRedis) Del(ctx, keys ...string) error {
    return a.c.Del(ctx, keys...).Err()
}
cache := cachelib.NewRedis(myRedis{c}, "order-core:pi", 5*time.Minute)
```

## Read-through + singleflight

```go
pi, err := cachelib.ReadThrough[*PaymentIntent](ctx, cache, "pi:"+id,
    func(ctx context.Context) (*PaymentIntent, error) {
        return repo.Get(ctx, id)  // DB load
    },
    cachelib.ReadThroughOptions{TTL: 5 * time.Minute},
)
```

- Cache miss + 50 并发 goroutine → 只有 1 个真调 loader, 其它等结果.
- Cache hit → 直接返回, loader 不调.
- Loader 错 → 透传, 不写缓存.
- Loader 返 ErrNotFound + `CacheNotFound: true` → 负缓存防热点穿透.

## Invalidate on write

```go
if err := repo.Update(ctx, pi); err != nil { return err }
_ = cachelib.Invalidate(ctx, cache, "pi:"+id)
```

## Backends

| Backend | When | Cross-replica? | Crash-safe? |
|---|---|---|---|
| `NewMemory(ttl)` | dev/单测/单副本 | ❌ | ❌ |
| `NewRedis(client, prefix, ttl)` | prod | ✅ | ✅ (rdb/AOF) |
| `NewNoop()` | 显式禁用 / 测试基线 | N/A | N/A |

## Why not just import go-redis?

payment-util 是底层公共包. 直接 import go-redis 会强制下游服务版本对齐 +
增大编译体积. 用 `RedisClient` 抽象后, 每个 consumer 自带 client + 一个
~20 行 adapter, payment-util 自身只依赖标准库.
