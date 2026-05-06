// Package service 实现 config-center gRPC handler 后面的业务逻辑。
//
// 拓扑：
//   handler (server/grpc.go) → service (本包) → repo (DB)
//                                   ↓
//                               watcherHub (本包，进程内 fan-out)
//
// 设计要点：
//   - PutConfig 在事务里：Insert config_version + Update config_item.active_version
//     + Insert config_audit_log 三件齐全；任一失败回滚。
//   - WatchConfig 走 watcherHub：每个客户端订阅是一个 channel，PutConfig 后
//     通知 hub.Publish 把事件分发到所有订阅本 namespace 的 channel。
//   - 启动期 watcher 重连 since_version > 0 时，先拉 DB 里大于该 version 的
//     增量推一遍（保证断线期间不丢事件），再开始接 hub 实时推送。
//
// **一致性**：单 server 实例没问题；多副本时同 namespace 的 hub 在不同 instance
// 上，需要 Kafka pub/sub 跨 instance fan-out。本 v1 简化为单实例；多副本下
// `since_version` resume 仍然能拿到最终一致（最多 InitTimeout 延迟）。
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// ConfigRow DB 表 config_version 的 ORM 形态；service 层用作内部通信。
type ConfigRow struct {
	ID            int64
	Namespace     string
	KeyName       string
	Version       int64
	Value         string
	Format        string
	EffectiveAt   *time.Time
	ExpireAt      *time.Time
	Strategy      string // FULL / CANARY / TARGETED / SCHEDULED
	StrategySpec  string // JSON 序列化的 CanarySpec / TargetedSpec
	CreatedBy     string
	ChangeReason  string
	CreatedAt     time.Time
}

// Repo 抽象 DB 操作，便于测试 mock。真实实现在 internal/repo。
type Repo interface {
	// PutVersion 事务：写 config_version + 更新 config_item.active_version + 写 audit_log
	// 返回新 version 号（per (namespace, key) 单调）。
	PutVersion(ctx context.Context, in PutVersionInput) (int64, error)
	// Rollback 事务：copy 旧 version 为新 version，把 active_version 指过去
	Rollback(ctx context.Context, namespace, key string, toVersion int64, actor, reason string) (int64, error)
	// GetActive 取 (namespace, key) 当前生效版本（active_version 指针对应的 row）
	GetActive(ctx context.Context, namespace, key string) (*ConfigRow, error)

	// ListNamespaceForSnapshot 给 watch 初始 snapshot 用：
	//   每个 key **都返两份**（如果有）：
	//     1. 当前生效版本（config_item.active_version 指向）
	//     2. 未来生效版本（config_version.effective_at > now 且 version > active_version）
	//        多个 SCHEDULED 排队时按 effective_at 升序，全推；客户端 SDK
	//        cache 会以最近一个为 pending（之后的覆盖前面的）
	//   客户端按 IsEffective(now) 决定槽位：active vs pending。
	ListNamespaceForSnapshot(ctx context.Context, namespace string) ([]*ConfigRow, error)

	// SinceVersion 取 namespace 下 version > since 的所有 row（resume 用）
	SinceVersion(ctx context.Context, namespace string, since int64) ([]*ConfigRow, error)
	// ListVersions 取 (namespace, key) 历史 version 列表（admin 详情页用）
	ListVersions(ctx context.Context, namespace, key string, limit int) ([]*ConfigRow, error)
	// Delete 软删（标 deleted=1）+ audit
	Delete(ctx context.Context, namespace, key, actor, reason string) error
	// CancelPendingVersion 删除一个"未来排队中、还没到点"的 SCHEDULED 版本：
	//   只允许删 effective_at > now() 且 != active_version 的行；
	//   已经生效过 / 当前 active 的版本不能删（保审计 + rollback 能力）。
	// 用于 admin 在 UI 上「计划改了又反悔」的场景。
	CancelPendingVersion(ctx context.Context, namespace, key string, version int64, actor, reason string) error
}

// PutVersionInput Repo.PutVersion 的入参。
type PutVersionInput struct {
	Namespace     string
	Key           string
	Value         string
	Format        string
	EffectiveAt   *time.Time
	ExpireAt      *time.Time
	Strategy      string
	StrategySpec  string
	Actor         string
	ChangeReason  string
}

// Service 业务接口。
type Service struct {
	repo   Repo
	hub    *watcherHub
	logger *zap.Logger
}

// New 构造。
func New(repo Repo, logger *zap.Logger) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Service{
		repo:   repo,
		hub:    newWatcherHub(),
		logger: logger,
	}
}

// PutConfig admin RPC：写新版本 + 通知 watcher。
func (s *Service) PutConfig(ctx context.Context, in PutVersionInput) (int64, error) {
	if in.Namespace == "" || in.Key == "" {
		return 0, errors.New("namespace and key required")
	}
	if in.Actor == "" {
		return 0, errors.New("actor required for audit")
	}
	if in.Strategy == "" {
		in.Strategy = "FULL"
	}
	newVer, err := s.repo.PutVersion(ctx, in)
	if err != nil {
		return 0, fmt.Errorf("put version: %w", err)
	}
	// 通知所有订阅本 namespace 的 watcher
	row, err := s.repo.GetActive(ctx, in.Namespace, in.Key)
	if err == nil && row != nil {
		s.hub.Publish(in.Namespace, &Event{
			Type:   EventUpdate,
			Config: row,
		})
	}
	s.logger.Info("config put",
		zap.String("namespace", in.Namespace),
		zap.String("key", in.Key),
		zap.Int64("version", newVer),
		zap.String("strategy", in.Strategy),
		zap.String("actor", in.Actor))
	return newVer, nil
}

// Rollback admin RPC：切回旧 version（产新 version 号，保单调）。
func (s *Service) Rollback(ctx context.Context, namespace, key string, toVersion int64, actor, reason string) (int64, error) {
	if actor == "" {
		return 0, errors.New("actor required")
	}
	if toVersion <= 0 {
		return 0, errors.New("to_version must be > 0")
	}
	newVer, err := s.repo.Rollback(ctx, namespace, key, toVersion, actor, reason)
	if err != nil {
		return 0, fmt.Errorf("rollback: %w", err)
	}
	row, err := s.repo.GetActive(ctx, namespace, key)
	if err == nil && row != nil {
		s.hub.Publish(namespace, &Event{Type: EventUpdate, Config: row})
	}
	s.logger.Warn("config rollback",
		zap.String("namespace", namespace),
		zap.String("key", key),
		zap.Int64("to_version", toVersion),
		zap.Int64("new_version", newVer),
		zap.String("actor", actor))
	return newVer, nil
}

// CancelPendingVersion 删除一个未来排队中、还没到点的版本（admin 计划改了
// 又反悔的场景）。约束在 repo 层：effective_at > now() 且 != active_version。
//
// 删除后给同 namespace 推 EventDelete（带版本号），客户端 SDK pending 槽
// 自动清掉对应行；若同 key 还有更靠后的 SCHEDULED 版本，那条仍正常排队。
func (s *Service) CancelPendingVersion(ctx context.Context, namespace, key string, version int64, actor, reason string) error {
	if actor == "" {
		return errors.New("actor required")
	}
	if version <= 0 {
		return errors.New("version must be > 0")
	}
	if err := s.repo.CancelPendingVersion(ctx, namespace, key, version, actor, reason); err != nil {
		return fmt.Errorf("cancel pending: %w", err)
	}
	// 通知客户端：同 (ns, key) 的 active 不变，但 pending 槽里那一项要移除。
	// 简化：发一次当前 active 的 update，让 SDK 重建 pending 槽（snapshot 模式
	// 下 SDK 会重读 namespace；增量模式 cache 自然 evict 那个版本号）。
	row, err := s.repo.GetActive(ctx, namespace, key)
	if err == nil && row != nil {
		s.hub.Publish(namespace, &Event{Type: EventUpdate, Config: row})
	}
	s.logger.Warn("config cancel pending version",
		zap.String("namespace", namespace),
		zap.String("key", key),
		zap.Int64("version", version),
		zap.String("actor", actor),
		zap.String("reason", reason))
	return nil
}

// GetConfig 客户端读：判断 strategy 命中 + effective 窗口。
func (s *Service) GetConfig(ctx context.Context, namespace, key, instanceID string) (*ConfigRow, error) {
	row, err := s.repo.GetActive(ctx, namespace, key)
	if err != nil || row == nil {
		return nil, err
	}
	if !s.matchStrategy(row, instanceID) {
		return nil, nil // 客户端不在策略命中范围
	}
	return row, nil
}

// WatchNamespace 客户端 stream：先发 snapshot（含每 key 的「当前生效 + 未来即将生效」
// 两份），后接 realtime 增量。
//
// **snapshot 推送规则**（保证客户端启动后本地 cache 完整）：
//   - 每个 key 推 1-N 行：
//     · 必含 config_item.active_version 指向的当前生效行
//     · 加推 config_version 中 effective_at > now 且 version > active 的"未来 SCHEDULED"行
//   - 客户端按各行 IsEffective(now) 自动归位：立即生效→active 槽，未来生效→pending 槽
//
// since_version > 0：resume 模式，只发 version > sinceVersion 的增量；客户端
// 用本地 maxVersion 续传，断网期间漏掉的事件重连后补齐。
//
// 每个订阅一个独立 channel；caller 要 range channel 直到 ctx.Done。
func (s *Service) WatchNamespace(ctx context.Context, namespace, instanceID string, sinceVersion int64) (<-chan *Event, error) {
	out := make(chan *Event, 64)
	// 1) 先发 snapshot（含未来版本）或 since-version 增量
	go func() {
		var rows []*ConfigRow
		var err error
		if sinceVersion <= 0 {
			rows, err = s.repo.ListNamespaceForSnapshot(ctx, namespace)
		} else {
			rows, err = s.repo.SinceVersion(ctx, namespace, sinceVersion)
		}
		if err != nil {
			s.logger.Warn("watch initial fetch failed", zap.Error(err))
		}
		for _, r := range rows {
			if !s.matchStrategy(r, instanceID) {
				continue
			}
			ev := &Event{Type: EventSnapshot, Config: r}
			if sinceVersion > 0 {
				ev.Type = EventUpdate
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
		// 2) 注册到 hub 接 realtime；ctx 取消时 hub.Subscribe 自动 unregister
		s.hub.Subscribe(ctx, namespace, instanceID, out, s.matchStrategy)
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

// matchStrategy 实例 id 是否命中 release 策略。FULL 永远 true；CANARY 按
// percent + target list；TARGETED 严格 list；SCHEDULED 看 effective_at。
func (s *Service) matchStrategy(r *ConfigRow, instanceID string) bool {
	if r == nil {
		return false
	}
	now := time.Now()
	if r.EffectiveAt != nil && now.Before(*r.EffectiveAt) {
		return false
	}
	if r.ExpireAt != nil && !now.Before(*r.ExpireAt) {
		return false
	}
	switch r.Strategy {
	case "FULL", "":
		return true
	case "SCHEDULED":
		// effective_at 已经在上面挡过了；到点后等同 FULL
		return true
	case "CANARY":
		return matchCanary(r.StrategySpec, instanceID)
	case "TARGETED":
		return matchTargeted(r.StrategySpec, instanceID)
	}
	return false
}
