// system_config_service.go：accounting-system 业务侧 key-value 配置入口。
//
// **历史**：旧实现在 meta DB 维了 system_config 表 + sync.RWMutex 内存 cache +
// admin POST /admin/reload/config 扇出。本质是个山寨 config-center。
//
// **现状（v2）**：完全删除 DB 表 + 自己的 cache + reload，直接 wrap 全平台
// config-center SDK（packages/config-center + payment-util/configcenter）。
//
// 业务调用层（accounting_service / outbox_worker / day_cut_scheduler 等）9 个
// 文件**保持不变**：仍然调 GetString / GetInt / GetBool / GetJSON，只是底层
// 数据来源从本地 DB 表换成了 config-center 推送的本地 cache（atomic.Pointer，
// 纳秒级零锁读）。
//
// **namespace 映射**：accounting-system 的所有 system_config key 在 config-center
// 里全部归到 namespace="accounting-system"，key 名 1:1 保留（如
// "tcc_recovery.stuck_timeout_minutes"）。
//
// **Upsert / Delete / Reload / ListAll**：
//   - 写操作：原 SystemConfigService.Upsert 现在是空操作（admin-web 的「修改
//     配置」按钮需要改成调 config-center 的 PUT /api/v1/configs/...）；为了
//     不破坏 adminhttp 的接口，保留方法签名但每次返 errOnboardingPending。
//   - Delete 同理。
//   - Reload 调 SDK 强制走一次 watch（重连）；但 SDK 本身在后台 streaming，
//     一般不需要主动 reload。
//   - ListAll 现在从 SDK 本地 cache 拿（受限于 namespace=accounting-system），
//     用于 admin-web 列表展示。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/payment-util/configcenter"
	"go.uber.org/zap"
)

// SystemConfigService 业务侧 key-value 接口；保持原签名不动，9 个 caller 零改动。
type SystemConfigService interface {
	// Reload 触发一次 SDK 同步刷新（一般不必，watch 已实时）。
	Reload(ctx context.Context) (int, error)
	// ListAll 当前本地 cache 全量。
	ListAll() []*model.SystemConfig
	// Upsert 写：v2 走 config-center HTTP PUT。底层 admin 调用不再走 DB。
	//
	// **deprecation**：admin-web 应直接调 config-center；本方法兼容
	// 保留旧调用站点（adminhttp）但建议清理。
	Upsert(ctx context.Context, key, valueJSON, valueType, description, updatedBy string) error
	// Delete 删一条 config。同样建议改用 config-center admin UI。
	Delete(ctx context.Context, key string) (bool, error)

	GetString(key, def string) string
	GetInt(key string, def int) int
	GetInt64(key string, def int64) int64
	GetBool(key string, def bool) bool
	GetFloat64(key string, def float64) float64
	GetJSON(key string, dst interface{}) bool
}

// systemConfigService 现在是 configcenter.Client 的薄薄一层 adapter。
type systemConfigService struct {
	cli       *configcenter.Client
	namespace string
	logger    *zap.Logger
	// 兼容旧 ListAll 用。SDK 内部 cache 是 unexported；这里维护一份 key 列表，
	// admin push 时通过 OnChange 回调累加。
	knownKeys map[string]struct{}
}

// NewSystemConfigService 构造。caller (main fx) 注入已 Init 的 configcenter.Client。
//
// 启动期 client 已经 InitialLoad 过；本服务直接可用。
func NewSystemConfigService(cli *configcenter.Client, logger *zap.Logger) SystemConfigService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &systemConfigService{
		cli:       cli,
		namespace: "accounting-system",
		logger:    logger,
		knownKeys: make(map[string]struct{}),
	}
}

// Reload SDK 后台 watch 已经实时；保留方法签名兼容 admin endpoint。
func (s *systemConfigService) Reload(_ context.Context) (int, error) {
	if s.cli == nil {
		return 0, errors.New("configcenter client not initialized")
	}
	// SDK 是 streaming pull；不暴露强制 reload。返当前 known key 数量代表「本地有 N 条」。
	return len(s.knownKeys), nil
}

// ListAll 返当前 SDK cache 中所有 known key（按 namespace 限定）。
//
// 老代码是从 sync.Map 拿 SystemConfig{ConfigKey, ValueJSON, ValueType, Description}；
// 新版只能拿到 key + value（描述 / 类型在 config-center admin DB 里）。
func (s *systemConfigService) ListAll() []*model.SystemConfig {
	if s.cli == nil {
		return nil
	}
	// 限制：SDK 不暴露 key 枚举接口（设计上 cache 是私有）；本服务维护 knownKeys
	// 集合，只能列出业务调用过 GetXxx 的 key。admin 列表查询应直接走
	// config-center admin UI（/admin/ns/accounting-system）。
	out := make([]*model.SystemConfig, 0, len(s.knownKeys))
	for k := range s.knownKeys {
		v, err := s.cli.Get(context.Background(), k)
		if err != nil || v == nil {
			continue
		}
		out = append(out, &model.SystemConfig{
			ConfigKey: k,
			ValueJSON: v.Value,
			ValueType: v.Format,
			UpdatedAt: v.UpdatedAt,
			UpdatedBy: v.UpdatedBy,
		})
	}
	return out
}

// errAdminMustUseConfigCenter 引导 admin 改走 config-center UI。
var errAdminMustUseConfigCenter = errors.New(
	"system_config 已迁到 config-center；admin 写操作请走 PUT /api/v1/configs/accounting-system/<key> " +
		"或直接用 config-center admin web (/admin/ns/accounting-system)")

// Upsert v2 拒绝；admin-web 应改调 config-center。
func (s *systemConfigService) Upsert(_ context.Context, key, _, _, _, _ string) error {
	return fmt.Errorf("upsert key=%q: %w", key, errAdminMustUseConfigCenter)
}

// Delete v2 拒绝。
func (s *systemConfigService) Delete(_ context.Context, key string) (bool, error) {
	return false, fmt.Errorf("delete key=%q: %w", key, errAdminMustUseConfigCenter)
}

// ─── 业务读路径（9 个 caller 走的 hot path） ─────────────────────────────

func (s *systemConfigService) GetString(key, def string) string {
	s.recordKey(key)
	if s.cli == nil {
		return def
	}
	return s.cli.GetString(context.Background(), key, def)
}

func (s *systemConfigService) GetInt(key string, def int) int {
	s.recordKey(key)
	if s.cli == nil {
		return def
	}
	return s.cli.GetInt(context.Background(), key, def)
}

func (s *systemConfigService) GetInt64(key string, def int64) int64 {
	s.recordKey(key)
	if s.cli == nil {
		return def
	}
	return s.cli.GetInt64(context.Background(), key, def)
}

func (s *systemConfigService) GetBool(key string, def bool) bool {
	s.recordKey(key)
	if s.cli == nil {
		return def
	}
	return s.cli.GetBool(context.Background(), key, def)
}

func (s *systemConfigService) GetFloat64(key string, def float64) float64 {
	s.recordKey(key)
	if s.cli == nil {
		return def
	}
	return s.cli.GetFloat64(context.Background(), key, def)
}

// GetJSON 把 value 反序列化到 dst。dst 必须是指针。
func (s *systemConfigService) GetJSON(key string, dst interface{}) bool {
	s.recordKey(key)
	if s.cli == nil {
		return false
	}
	// configcenter 提供泛型 GetJSON；这里是 interface{} dst，老版兼容签名，
	// 用 v.Value 自己 unmarshal（SDK 的 GetJSON 是泛型 T any，类型擦除场景
	// 走 client.Get + json.Unmarshal）
	v, err := s.cli.Get(context.Background(), key)
	if err != nil || v == nil {
		return false
	}
	if err := json.Unmarshal([]byte(v.Value), dst); err != nil {
		s.logger.Warn("system_config GetJSON unmarshal failed",
			zap.String("key", key), zap.Error(err))
		return false
	}
	return true
}

// recordKey 把 caller 调过的 key 记下，给 ListAll 用。
func (s *systemConfigService) recordKey(key string) {
	s.knownKeys[key] = struct{}{}
}
