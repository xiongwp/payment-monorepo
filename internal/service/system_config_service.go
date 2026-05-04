package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/repository"
	"go.uber.org/zap"
)

// SystemConfigService 通用 key-value 配置中心。
//
// 设计理念：
//   - 所有可调参数（TCC stuck timeout、outbox 轮询间隔、限流阈值 …）从硬编码 / yaml
//     迁移到 meta DB，admin-web 可在线编辑、推送到所有实例。
//   - value 用 JSON 序列化（string / int / bool / array / object 都行）。
//   - 服务侧：启动时 Reload 全量进内存 map；admin 写后扇出 /admin/reload/config；
//     60s 兜底 tick 防扇出失败。
//   - 读路径：业务代码直接调 GetInt/GetString/GetBool/GetJSON("key", default)，
//     从 RWMutex 保护的 map 拿值，O(1) 无锁竞争（读多写少）。
type SystemConfigService interface {
	// Reload 从 DB 全量读到内存（覆盖现有 cache）。启动时 + 周期 + admin push 都调它。
	Reload(ctx context.Context) (int, error)
	// ListAll 返回当前内存里全部配置（admin-web 列表展示用）。
	ListAll() []*model.SystemConfig
	// Upsert 写 DB 并立即更新本地 cache。Fanout 由 admin-web 端做。
	Upsert(ctx context.Context, key, valueJSON, valueType, description, updatedBy string) error
	// Delete 删除配置。
	Delete(ctx context.Context, key string) (bool, error)

	// 类型化读取：从 cache 拿值，按 type 解析。miss / 解析失败 → 返回 default。
	// 业务代码通过这些方法读，不直接接 SystemConfig。
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetInt64(key string, def int64) int64
	GetBool(key string, def bool) bool
	GetFloat64(key string, def float64) float64
	// GetJSON 把 value_json 反序列化到 dst（dst 必须是指针）。
	// miss / 解析失败返回 false（dst 不被改写）。
	GetJSON(key string, dst interface{}) bool
}

type systemConfigService struct {
	repo   repository.SystemConfigRepository
	logger *zap.Logger
	mu     sync.RWMutex
	cache  map[string]*model.SystemConfig
}

func NewSystemConfigService(repo repository.SystemConfigRepository, logger *zap.Logger) SystemConfigService {
	return &systemConfigService{
		repo:   repo,
		logger: logger,
		cache:  make(map[string]*model.SystemConfig),
	}
}

func (s *systemConfigService) Reload(ctx context.Context) (int, error) {
	rows, err := s.repo.ListAll(ctx)
	if err != nil {
		return 0, err
	}
	newCache := make(map[string]*model.SystemConfig, len(rows))
	for _, r := range rows {
		newCache[r.ConfigKey] = r
	}
	s.mu.Lock()
	s.cache = newCache
	s.mu.Unlock()
	return len(rows), nil
}

func (s *systemConfigService) ListAll() []*model.SystemConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.SystemConfig, 0, len(s.cache))
	for _, c := range s.cache {
		out = append(out, c)
	}
	return out
}

func (s *systemConfigService) Upsert(ctx context.Context, key, valueJSON, valueType, description, updatedBy string) error {
	if key == "" {
		return fmt.Errorf("config_key required")
	}
	// 校验 value_json 真的是合法 JSON（避免 admin 传 raw 字符串崩溃所有 reader）
	var any interface{}
	if err := json.Unmarshal([]byte(valueJSON), &any); err != nil {
		return fmt.Errorf("value_json not valid JSON: %w", err)
	}
	cfg := &model.SystemConfig{
		ConfigKey:   key,
		ValueJSON:   valueJSON,
		ValueType:   valueType,
		Description: description,
		UpdatedBy:   updatedBy,
		UpdatedAt:   time.Now(),
	}
	if err := s.repo.Upsert(ctx, cfg); err != nil {
		return err
	}
	// 写完立即更新本地 cache（其他实例靠 fanout reload）
	s.mu.Lock()
	s.cache[key] = cfg
	s.mu.Unlock()
	return nil
}

func (s *systemConfigService) Delete(ctx context.Context, key string) (bool, error) {
	deleted, err := s.repo.Delete(ctx, key)
	if err != nil {
		return false, err
	}
	if deleted {
		s.mu.Lock()
		delete(s.cache, key)
		s.mu.Unlock()
	}
	return deleted, nil
}

// 内部 helper：取 raw JSON 字符串
func (s *systemConfigService) raw(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cache[key]
	if !ok {
		return "", false
	}
	return c.ValueJSON, true
}

func (s *systemConfigService) GetString(key, def string) string {
	v, ok := s.raw(key)
	if !ok {
		return def
	}
	// 数据存的可能是 "abc" 或 abc；先尝试 unquote 当 JSON 字符串
	var str string
	if err := json.Unmarshal([]byte(v), &str); err == nil {
		return str
	}
	return v
}

func (s *systemConfigService) GetInt(key string, def int) int {
	v, ok := s.raw(key)
	if !ok {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

func (s *systemConfigService) GetInt64(key string, def int64) int64 {
	v, ok := s.raw(key)
	if !ok {
		return def
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	return def
}

func (s *systemConfigService) GetBool(key string, def bool) bool {
	v, ok := s.raw(key)
	if !ok {
		return def
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return def
}

func (s *systemConfigService) GetFloat64(key string, def float64) float64 {
	v, ok := s.raw(key)
	if !ok {
		return def
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return def
}

func (s *systemConfigService) GetJSON(key string, dst interface{}) bool {
	v, ok := s.raw(key)
	if !ok {
		return false
	}
	if err := json.Unmarshal([]byte(v), dst); err != nil {
		s.logger.Warn("system_config GetJSON unmarshal failed",
			zap.String("key", key), zap.Error(err))
		return false
	}
	return true
}
