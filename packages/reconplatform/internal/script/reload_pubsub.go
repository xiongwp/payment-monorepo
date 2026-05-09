// reload_pubsub.go — 多实例脚本热刷。
//
// 问题：reconplatform 部署多副本时，admin 在副本 A 保存脚本，loader.Replace
// 只更新了 A 的内存。副本 B / C 仍跑老版直到下次 Restart。
//
// 解决：Redis Pub/Sub。SaveDef 后 publish "recon:script:reload" 事件
// (script_id + version)，所有 reconplatform 实例 subscribe 这个 channel，
// 收到后从 Redis 拉最新代码 loader.Upsert。
//
// 设计权衡：
//
//   - **不用 Kafka**：Pub/Sub 不需要持久化（副本短暂掉线就用启动期 reload from
//     Redis 兜底）；Kafka 是 overkill。
//   - **不轮询**：30s 轮询 Redis 全量也行但 admin 保存后 30s 才生效用户体验差；
//     Pub/Sub 平均 < 100ms 生效。
//   - **重复消费 OK**：一个事件被同实例收 N 次 / 收到无效 ID 都没问题，
//     loader.Upsert 是幂等的；Replace 失败的脚本不会卸载老版（保留旧编译产物）。
//   - **leader-follower 不需要**：每个实例独立处理，互不阻塞。
//
// 故障模式：
//
//   - Redis 抖断：subscribe 自动重连（go-redis 内置）；重连后已错过的事件丢失，
//     由启动期 reload from Redis（main.go 已有的逻辑）兜底。多副本一起重启时，
//     最差也是各副本各自从 Redis 拉一次最新版。
//   - 脚本编译失败：PublishReload 会广播一个无效版本，所有实例 Upsert 都失败；
//     log 里 warn 但服务不挂。admin web 看本副本的报错决定是否回滚。

package script

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// reloadChannel Redis Pub/Sub channel 名。
const reloadChannel = "recon:script:reload"

// reloadEvent Pub/Sub 消息体。
type reloadEvent struct {
	ScriptID string `json:"script_id"`
	Action   string `json:"action"`            // "save" / "delete"
	Version  int64  `json:"version,omitempty"` // 给排查用，loader 不依赖这个值
	By       string `json:"by,omitempty"`
	TS       int64  `json:"ts,omitempty"` // unix ms
}

// PublishReload 把"该脚本被改了"广播给所有 reconplatform 实例。
// 由 SaveDef / DeleteDef 在持久化成功后调用。
//
// 失败不返 err（仅 log）：发布失败不应阻塞 admin 保存——事件丢了下次 admin
// 改 / 启动期 reload 会兜底。
func (s *Store) PublishReload(ctx context.Context, scriptID, action, by string, version int64) {
	evt := reloadEvent{
		ScriptID: scriptID,
		Action:   action,
		Version:  version,
		By:       by,
		TS:       time.Now().UnixMilli(),
	}
	b, _ := json.Marshal(evt)
	_ = s.r.Publish(ctx, reloadChannel, b).Err()
}

// ReloadWatcher 在每个 reconplatform 实例启动期起一个 goroutine
// subscribe reloadChannel，收到事件后从 Redis 拉最新代码并 loader.Upsert。
//
// caller 在 main.go 里：
//
//	watcher := script.NewReloadWatcher(rdb, store, loader, logger)
//	go watcher.Run(ctx)
//
// ctx canceled → 自动退出。
type ReloadWatcher struct {
	r      redis.UniversalClient
	store  *Store
	loader *Loader
	log    *zap.Logger
}

// NewReloadWatcher 构造。
func NewReloadWatcher(r redis.UniversalClient, store *Store, loader *Loader, log *zap.Logger) *ReloadWatcher {
	if log == nil {
		log = zap.NewNop()
	}
	return &ReloadWatcher{r: r, store: store, loader: loader, log: log}
}

// Run 阻塞直到 ctx canceled。Pub/Sub 自动重连（go-redis 行为）。
func (w *ReloadWatcher) Run(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		err := w.runOnce(ctx)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			w.log.Warn("reload watcher loop error, reconnecting in 2s", zap.Error(err))
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

func (w *ReloadWatcher) runOnce(ctx context.Context) error {
	pubsub := w.r.Subscribe(ctx, reloadChannel)
	defer pubsub.Close()

	// 先 Receive 一次确认连上（go-redis 推荐做法）
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	w.log.Info("reload watcher subscribed", zap.String("channel", reloadChannel))

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return context.Canceled
		case msg, ok := <-ch:
			if !ok {
				return errors.New("pubsub channel closed")
			}
			w.handleMessage(ctx, msg.Payload)
		}
	}
}

func (w *ReloadWatcher) handleMessage(ctx context.Context, payload string) {
	var evt reloadEvent
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		w.log.Warn("invalid reload payload, skipping",
			zap.String("payload", payload), zap.Error(err))
		return
	}
	if evt.ScriptID == "" {
		return
	}
	switch evt.Action {
	case "delete":
		w.loader.Remove(evt.ScriptID)
		w.log.Info("script removed by remote",
			zap.String("script_id", evt.ScriptID),
			zap.String("by", evt.By))
	default: // save / 默认按更新处理
		def, err := w.store.LoadDef(ctx, evt.ScriptID)
		if err != nil || def == nil {
			w.log.Warn("reload watcher: LoadDef failed",
				zap.String("script_id", evt.ScriptID), zap.Error(err))
			return
		}
		// LoadDef 直接返 *Script；hot reload 期 loader.Upsert 会重新编译。
		if err := w.loader.Upsert(evt.ScriptID, def); err != nil {
			w.log.Warn("reload watcher: Upsert failed (kept old version)",
				zap.String("script_id", evt.ScriptID), zap.Error(err))
			return
		}
		w.log.Info("script reloaded by remote",
			zap.String("script_id", evt.ScriptID),
			zap.Int64("version", evt.Version),
			zap.String("by", evt.By))
	}
}
