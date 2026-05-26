// Stub for Redis session Store；默认构建不引入 redis 依赖。开 -tags redis
// 才会用 redis_store.go 替代本文件。

//go:build !redis

package session

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
)

// ErrRedisNotEnabled 标记未启用 redis build tag；所有方法都返这个 sentinel。
var ErrRedisNotEnabled = errors.New("session: redis build tag not enabled")

// RedisStore stub；新建时不报错（让 wire 能跑通），所有方法返 ErrRedisNotEnabled。
type RedisStore struct{}

// NewRedisStore stub 工厂；接受 *redis.Client 参数对齐真实签名。
func NewRedisStore(_ *redis.Client) *RedisStore { return &RedisStore{} }

func (r *RedisStore) Create(_ Snapshot) (string, error) {
	return "", ErrRedisNotEnabled
}

func (r *RedisStore) Finalize(_ string, _ BehaviorPatch) error {
	return ErrRedisNotEnabled
}

func (r *RedisStore) Get(_ string) *Snapshot { return nil }

func (r *RedisStore) Erase(_ context.Context, _ string) error {
	return ErrRedisNotEnabled
}
