// Redis 实现：开 build tag `redis` 后才编译进二进制（同 store/redis_counter）。

//go:build redis

package featurecache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache 跨进程共享缓存。失败 fail-open（read miss / write no-op）。
type RedisCache struct {
	rdb        *redis.Client
	prefix     string
	defaultTTL time.Duration
}

func NewRedisCache(rdb *redis.Client, prefix string, defaultTTL time.Duration) *RedisCache {
	if prefix == "" {
		prefix = "risk:feat:"
	}
	if defaultTTL <= 0 {
		defaultTTL = 5 * time.Minute
	}
	return &RedisCache{rdb: rdb, prefix: prefix, defaultTTL: defaultTTL}
}

func (c *RedisCache) k(key string) string { return fmt.Sprintf("%s{%s}", c.prefix, key) }

func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, bool) {
	v, err := c.rdb.Get(ctx, c.k(key)).Bytes()
	if err != nil || len(v) == 0 {
		return nil, false
	}
	return v, true
}

func (c *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	_ = c.rdb.Set(ctx, c.k(key), value, ttl).Err()
}

func (c *RedisCache) Delete(ctx context.Context, key string) {
	_ = c.rdb.Del(ctx, c.k(key)).Err()
}
