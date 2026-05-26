// Redis 实现的 session Store；开 build tag `redis` 后才编译进二进制。
//
// 拓扑：
//   - Snapshot 存为 HSET key=risk:sess:<id>，字段对应 Snapshot 的 JSON tag
//   - EXPIRE 1800s（30min；和 MemStore 一致）
//   - customer secondary index：risk:sess:customer:<customer_id> 是 SET<session_id>，
//     SADD 同 TTL 让索引不长久残留
//   - Erase：SMEMBERS index → DEL keys + SREM + DEL index
//
// 多副本环境的 nonce 防重放仍走 http.go 的进程内 LRU；如果上 K8s 多副本 + 想严格
// 防 cross-pod replay，应额外加 redis.SetNX EX 60 一层（与本 Store 解耦，留待后续）。

//go:build redis

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisKeyPrefix      = "risk:sess:"
	redisCustomerPrefix = "risk:sess:customer:"
	redisSessionTTL     = 30 * time.Minute
)

// ErrRedisNotEnabled 占位；redis build 下永远不返。stub 版本会返这个。
var ErrRedisNotEnabled = errors.New("session: redis build tag not enabled")

// RedisStore 用 go-redis/v9 实现 Store。
type RedisStore struct {
	rdb *redis.Client
}

// NewRedisStore 工厂；rdb 必须已 ping 通，Store 不做 connection retry。
func NewRedisStore(rdb *redis.Client) *RedisStore {
	return &RedisStore{rdb: rdb}
}

func sessKey(id string) string         { return redisKeyPrefix + id }
func customerKey(cid string) string    { return redisCustomerPrefix + cid }

// Create HSET 字段 + EXPIRE。customer_id 非空时同步加 SET 索引。
func (r *RedisStore) Create(s Snapshot) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	s.SessionID = id
	s.CreatedAt = now
	s.UpdatedAt = now
	NormalizeForStorage(&s)
	ctx := context.Background()

	fields, err := snapshotToMap(&s)
	if err != nil {
		return "", err
	}
	pipe := r.rdb.TxPipeline()
	pipe.HSet(ctx, sessKey(id), fields)
	pipe.Expire(ctx, sessKey(id), redisSessionTTL)
	if s.CustomerID != "" {
		pipe.SAdd(ctx, customerKey(s.CustomerID), id)
		pipe.Expire(ctx, customerKey(s.CustomerID), redisSessionTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (r *RedisStore) Finalize(id string, b BehaviorPatch) error {
	ctx := context.Background()
	exists, err := r.rdb.Exists(ctx, sessKey(id)).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound{ID: id}
	}
	pasted, err := json.Marshal(b.PastedFields)
	if err != nil {
		return err
	}
	fields := map[string]any{
		"timeToCheckoutMs":     b.TimeToCheckoutMs,
		"mouseMovementEntropy": b.MouseMovementEntropy,
		"clickIntervalMs":      b.ClickIntervalMs,
		"scrollSpeedPxPerSec":  b.ScrollSpeedPxPerSec,
		"typingRhythmCV":       b.TypingRhythmCV,
		"keystrokeCount":       b.KeystrokeCount,
		"mouseMoves":           b.MouseMoves,
		"pastedFields":         string(pasted),
		"updated_at":           time.Now().UTC().Format(time.RFC3339Nano),
	}
	return r.rdb.HSet(ctx, sessKey(id), fields).Err()
}

func (r *RedisStore) Get(id string) *Snapshot {
	ctx := context.Background()
	m, err := r.rdb.HGetAll(ctx, sessKey(id)).Result()
	if err != nil || len(m) == 0 {
		return nil
	}
	s, err := mapToSnapshot(m)
	if err != nil {
		return nil
	}
	return s
}

// Erase 删除 customer 关联的所有 session + secondary index。
func (r *RedisStore) Erase(ctx context.Context, customerID string) error {
	if customerID == "" {
		return nil
	}
	ids, err := r.rdb.SMembers(ctx, customerKey(customerID)).Result()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		// 索引为空也 DEL 一次防遗留
		_ = r.rdb.Del(ctx, customerKey(customerID)).Err()
		return nil
	}
	keys := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		keys = append(keys, sessKey(id))
	}
	keys = append(keys, customerKey(customerID))
	return r.rdb.Del(ctx, keys...).Err()
}

// snapshotToMap 用 json 序列化 → map[string]any 喂 HSET。
// HSET 支持基本类型；slice / time 走 json 字符串。
func snapshotToMap(s *Snapshot) (map[string]any, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		out[k] = string(v)
	}
	return out, nil
}

func mapToSnapshot(m map[string]string) (*Snapshot, error) {
	// 把 string 重新组成 JSON 对象再解一次，避免每个字段写转换
	merged := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		merged[k] = json.RawMessage(v)
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("redis store decode: %w", err)
	}
	return &s, nil
}
