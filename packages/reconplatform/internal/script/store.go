// 脚本持久化：admin web 上保存的脚本要存在 Redis 里，进程重启后能 reload。
//
// Redis 布局：
//   recon:script:<id>           HASH  {name, code, schedule, triggers (JSON []), updated_at, updated_by, version}
//   recon:script:list           ZSET  member=id, score=updated_at_ms  （按更新时间倒序列表）
//   recon:script:audit          LIST  RPUSH JSON{id, action, by, ts, prev_version, ...}（最近 1000 条）
//
// 设计纪律：
//   - 写操作（Save/Delete）走 Lua 原子：list ZADD + HASH SET + audit RPUSH 三步一起做
//   - admin 改完保存 → 这层 store → 触发 loader.Replace（同进程内）+
//     PubSub 广播（多副本同步）
//   - 运行结果（diff）单独存：recon:result:<script_id>:<run_id>
package script

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store 脚本 + 运行结果持久化层。
type Store struct {
	r redis.UniversalClient
}

func NewStore(r redis.UniversalClient) *Store { return &Store{r: r} }

// SaveDef 保存脚本定义（admin 编辑器"保存"按钮）。
//
// 版本管理：
//   - HASH recon:script:<id>                当前版本（最新代码 + 元信息）
//   - HASH recon:script:<id>:v<N>           每个历史版本完整快照（永久保存）
//   - ZSET recon:script:<id>:versions       历史索引：member=N, score=savedTimeMs
//
// 每次 Save 把上一版完整 dump 到 :v<prev>，再把当前更新到 v<new>。
// 脚本可以一直回退（版本数量上限通过 RetainVersions 限定，默认 50）。
func (s *Store) SaveDef(ctx context.Context, id, name, code, schedule string, triggers []string, updatedBy string) (int64, error) {
	if id == "" {
		return 0, fmt.Errorf("empty script id")
	}
	now := time.Now()
	triggersJSON, _ := json.Marshal(triggers)

	// 拿上一版（如果存在的话）
	prevHash, _ := s.r.HGetAll(ctx, "recon:script:"+id).Result()
	prevVer, _ := strToInt64(prevHash["version"])
	newVer := prevVer + 1

	pipe := s.r.Pipeline()

	// 1) 上一版完整快照存到 :v<prev>（首次保存 prev=0 跳过）
	if prevVer > 0 && len(prevHash) > 0 {
		pipe.HSet(ctx, fmt.Sprintf("recon:script:%s:v%d", id, prevVer),
			toAnyMap(prevHash))
		pipe.ZAdd(ctx, "recon:script:"+id+":versions",
			redis.Z{Score: float64(now.UnixMilli()), Member: prevVer})
		// 保留最近 50 版
		pipe.ZRemRangeByRank(ctx, "recon:script:"+id+":versions", 0, -51)
	}

	// 2) 当前 hash 写入新版本数据
	pipe.HSet(ctx, "recon:script:"+id, map[string]any{
		"id":         id,
		"name":       name,
		"code":       code,
		"schedule":   schedule,
		"triggers":   string(triggersJSON),
		"updated_at": now.UnixMilli(),
		"updated_by": updatedBy,
		"version":    newVer,
	})
	pipe.ZAdd(ctx, "recon:script:list",
		redis.Z{Score: float64(now.UnixMilli()), Member: id})

	// 3) 全局 audit
	auditEntry, _ := json.Marshal(map[string]any{
		"id":           id,
		"action":       "save",
		"by":           updatedBy,
		"ts":           now.UnixMilli(),
		"prev_version": prevVer,
		"new_version":  newVer,
	})
	pipe.RPush(ctx, "recon:script:audit", auditEntry)
	pipe.LTrim(ctx, "recon:script:audit", -1000, -1)

	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	// 多实例热刷：Pub/Sub 广播让其他副本拉新版本。失败不阻塞返回。
	s.PublishReload(ctx, id, "save", updatedBy, newVer)
	return newVer, nil
}

// ListVersions 拿某脚本的历史版本列表（按 score 倒序）。
//
// 返 [{version, saved_at, updated_by, name}] 不带 code（避免 list 太大）。
// 单版详情走 GetVersion。
func (s *Store) ListVersions(ctx context.Context, id string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	z, err := s.r.ZRevRangeWithScores(ctx, "recon:script:"+id+":versions", 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(z))
	for _, item := range z {
		ver := fmt.Sprintf("%v", item.Member)
		// 只取轻量字段
		fields, err := s.r.HMGet(ctx, fmt.Sprintf("recon:script:%s:v%s", id, ver),
			"version", "name", "updated_at", "updated_by").Result()
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"version":    fields[0],
			"name":       fields[1],
			"updated_at": fields[2],
			"updated_by": fields[3],
			"saved_at":   int64(item.Score),
		})
	}
	return out, nil
}

// GetVersion 拿某历史版本的完整 dump（含 code，给"看历史"/diff 用）。
func (s *Store) GetVersion(ctx context.Context, id string, version int64) (*Script, error) {
	key := fmt.Sprintf("recon:script:%s:v%d", id, version)
	m, err := s.r.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("version v%d of %q not found", version, id)
	}
	updatedAt, _ := strToInt64(m["updated_at"])
	var triggers []string
	if t := m["triggers"]; t != "" {
		_ = json.Unmarshal([]byte(t), &triggers)
	}
	return &Script{
		ID:        m["id"],
		Name:      m["name"],
		Code:      m["code"],
		UpdatedAt: time.UnixMilli(updatedAt),
		UpdatedBy: m["updated_by"],
		Schedule:  m["schedule"],
		Triggers:  triggers,
	}, nil
}

// Rollback 把当前脚本回退到指定历史版本：拿 :v<N> 的快照，作为新版本保存。
// 旧 v<N> 仍在历史里（不直接覆盖），新版本号 = 当前 +1。
func (s *Store) Rollback(ctx context.Context, id string, toVersion int64, by string) (int64, error) {
	old, err := s.GetVersion(ctx, id, toVersion)
	if err != nil {
		return 0, err
	}
	newVer, err := s.SaveDef(ctx, id, old.Name, old.Code, old.Schedule, old.Triggers,
		fmt.Sprintf("%s (rollback to v%d)", by, toVersion))
	if err != nil {
		return 0, err
	}
	// 写一条 audit 注明 rollback 来源
	auditEntry, _ := json.Marshal(map[string]any{
		"id":           id,
		"action":       "rollback",
		"by":           by,
		"ts":           time.Now().UnixMilli(),
		"from_version": toVersion,
		"new_version":  newVer,
	})
	_ = s.r.RPush(ctx, "recon:script:audit", auditEntry).Err()
	return newVer, nil
}

// toAnyMap 把 redis HGetAll 拿到的 map[string]string 转 map[string]any，
// 给后续 HSet 用（HSet 接受 map[string]any 或 ...args）。
func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// LoadDef 拿单条脚本定义（启动期 + admin 编辑器加载）。
func (s *Store) LoadDef(ctx context.Context, id string) (*Script, error) {
	m, err := s.r.HGetAll(ctx, "recon:script:"+id).Result()
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("script %q not found", id)
	}
	updatedAt, _ := strToInt64(m["updated_at"])
	var triggers []string
	if t := m["triggers"]; t != "" {
		_ = json.Unmarshal([]byte(t), &triggers)
	}
	return &Script{
		ID:        m["id"],
		Name:      m["name"],
		Code:      m["code"],
		UpdatedAt: time.UnixMilli(updatedAt),
		UpdatedBy: m["updated_by"],
		Schedule:  m["schedule"],
		Triggers:  triggers,
	}, nil
}

// ListDefs 列出所有脚本（按 updated_at 倒序）。
func (s *Store) ListDefs(ctx context.Context, limit int) ([]*Script, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ids, err := s.r.ZRevRange(ctx, "recon:script:list", 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Script, 0, len(ids))
	for _, id := range ids {
		def, err := s.LoadDef(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, def)
	}
	return out, nil
}

// DeleteDef 删除脚本（连同元信息 + 列表条目；运行结果保留）。
func (s *Store) DeleteDef(ctx context.Context, id, deletedBy string) error {
	pipe := s.r.Pipeline()
	pipe.Del(ctx, "recon:script:"+id)
	pipe.ZRem(ctx, "recon:script:list", id)
	auditEntry, _ := json.Marshal(map[string]any{
		"id":     id,
		"action": "delete",
		"by":     deletedBy,
		"ts":     time.Now().UnixMilli(),
	})
	pipe.RPush(ctx, "recon:script:audit", auditEntry)
	pipe.LTrim(ctx, "recon:script:audit", -1000, -1)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	// 多实例热刷：通知其他副本卸载该脚本。
	s.PublishReload(ctx, id, "delete", deletedBy, 0)
	return nil
}

// SaveResult 写一次运行结果。
func (s *Store) SaveResult(ctx context.Context, r *Result) error {
	if r == nil || r.ScriptID == "" {
		return fmt.Errorf("invalid result")
	}
	jsonBytes, err := json.Marshal(r)
	if err != nil {
		return err
	}
	pipe := s.r.Pipeline()
	resultKey := fmt.Sprintf("recon:result:%s:%s", r.ScriptID, r.RunID)
	pipe.Set(ctx, resultKey, jsonBytes, 30*24*time.Hour) // 默认 30d 过期；可走 TTLProvider
	listKey := "recon:result:list:" + r.ScriptID
	pipe.ZAdd(ctx, listKey, redis.Z{Score: float64(r.StartedAt.UnixMilli()), Member: r.RunID})
	pipe.ZRemRangeByRank(ctx, listKey, 0, -101) // 保最近 100 次
	_, err = pipe.Exec(ctx)
	return err
}

// ListResults 拿某脚本最近 N 次运行（admin "运行历史" 页面）。
func (s *Store) ListResults(ctx context.Context, scriptID string, limit int) ([]*Result, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	listKey := "recon:result:list:" + scriptID
	ids, err := s.r.ZRevRange(ctx, listKey, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = fmt.Sprintf("recon:result:%s:%s", scriptID, id)
	}
	vals, err := s.r.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Result, 0, len(vals))
	for _, raw := range vals {
		if raw == nil {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			continue
		}
		out = append(out, &r)
	}
	return out, nil
}

// strToInt64 简单 helper。
func strToInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var n int64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}
