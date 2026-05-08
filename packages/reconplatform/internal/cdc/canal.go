// Package cdc — Canal binlog 订阅。
//
// 选型：github.com/go-mysql-org/go-mysql/canal 是 Go 生态最稳定的 MySQL
// binlog 客户端，伪装成 replica，订阅 ROW 格式的 INSERT/UPDATE/DELETE。
//
// 一个 reconplatform 进程可以同时跑 N 个 Runner（每服务一个），各自维护
// 自己的 binlog 位点。位点持久化到 Redis (recon:cdc:pos:<svc>)。
//
// 注意：本文件目前是 *骨架*。canal 库会作为新 dependency 引入；先把 Run /
// onRowsEvent 等接口写好，等 build 通过再开始接真 binlog。

package cdc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Runner 单服务的 binlog 摄入循环。Start 启动后阻塞，Stop 触发 graceful shutdown。
//
//	r := NewRunner(source, publisher, schemaProvider, logger)
//	go r.Run(ctx)
//	... shutdown:
//	r.Stop()
type Runner struct {
	source    Source
	publisher *Publisher
	schema    SchemaProvider
	logger    *zap.Logger

	mu       sync.Mutex
	canal    canalLike // 真实是 *canal.Canal；测试可注入 stub
	stopCh   chan struct{}
	stopped  bool

	// 上次 flush 位点的时间，避免每条事件都 fsync
	lastPosFlush time.Time
}

// canalLike canal.Canal 的子集接口，便于测试 / 延迟引入真实依赖。
type canalLike interface {
	Run() error
	Close()
}

// SchemaProvider parser 用：拿到表的列名 / 类型。由 meta 包提供。
type SchemaProvider interface {
	Columns(service, schema, table string) ([]columnInfo, error)
}

// NewRunner 构造，但不立刻拨号 binlog（Run 时才 dial）。
func NewRunner(src Source, pub *Publisher, sp SchemaProvider, logger *zap.Logger) *Runner {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Runner{
		source:    src,
		publisher: pub,
		schema:    sp,
		logger:    logger.With(zap.String("svc", src.Service)),
		stopCh:    make(chan struct{}),
	}
}

// Run 阻塞直到 ctx 取消 / Stop / canal 出错。
//
// 真实实现里：
//
//	1. 用 source 配置初始化 canal.NewCanal
//	2. canal.SetEventHandler 挂自定义 handler，OnRow 里调 parseRow + publisher.Publish
//	3. 上次保存的位点从 publisher.LoadPosition 拿；没记录就从 latest binlog 起跑
//	4. Run() 阻塞订阅
//
// 当前为骨架：返回未实现错误，主进程降级跑（不影响其他服务部署）。
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return errors.New("runner: already stopped")
	}
	r.mu.Unlock()

	r.logger.Info("cdc runner: start (skeleton; canal not yet wired)",
		zap.String("addr", r.source.Addr),
		zap.Int("tables", len(r.source.Tables)))

	// 等到真集成 canal 之前，先做：
	//   1. LoadPosition 验通
	//   2. 周期 noop 心跳，让进程能起来不卡死
	file, pos, gtid, ok := r.publisher.LoadPosition(ctx, r.source.Service)
	if ok {
		r.logger.Info("loaded last binlog position",
			zap.String("file", file), zap.Uint32("pos", pos), zap.String("gtid", gtid))
	} else {
		r.logger.Info("no saved position; will start from latest binlog when canal is wired")
	}

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.stopCh:
			return nil
		case <-tick.C:
			r.logger.Debug("cdc runner: heartbeat (skeleton)")
		}
	}
}

// Stop 触发 graceful shutdown；Run 会在下个 select 周期返回。
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.stopped = true
	close(r.stopCh)
	if r.canal != nil {
		r.canal.Close()
	}
}

// flushPositionThrottled 每秒最多写一次 binlog 位点。canal handler 里调，
// 避免每条事件都 fsync redis。
func (r *Runner) flushPositionThrottled(ctx context.Context, file string, pos uint32, gtid string) {
	r.mu.Lock()
	if time.Since(r.lastPosFlush) < time.Second {
		r.mu.Unlock()
		return
	}
	r.lastPosFlush = time.Now()
	r.mu.Unlock()

	if err := r.publisher.SavePosition(ctx, r.source.Service, file, pos, gtid); err != nil {
		r.logger.Warn("save binlog position failed", zap.Error(err))
	}
}

// Manager 一份 sources 配置 → N 个 Runner，统一管理生命周期。
type Manager struct {
	publisher *Publisher
	schema    SchemaProvider
	logger    *zap.Logger

	mu      sync.Mutex
	runners map[string]*Runner // svc → runner
	wg      sync.WaitGroup
}

// NewManager 构造，sources 通过 Reload 注入（启动期 + OnChange 都用同一个入口）。
func NewManager(pub *Publisher, sp SchemaProvider, logger *zap.Logger) *Manager {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Manager{
		publisher: pub,
		schema:    sp,
		logger:    logger,
		runners:   make(map[string]*Runner),
	}
}

// Reload diff 当前 runners vs 新 sources：
//   - 新增的 service 启 runner
//   - 已删除的停 runner
//   - 已存在但配置变了的：停旧的，启新的（暂不支持原地热更，等 canal 接进来再细化）
func (m *Manager) Reload(ctx context.Context, sources []Source) {
	m.mu.Lock()
	defer m.mu.Unlock()

	want := make(map[string]Source, len(sources))
	for _, s := range sources {
		want[s.Service] = s
	}

	// 停掉不再需要的
	for svc, r := range m.runners {
		if _, keep := want[svc]; !keep {
			m.logger.Info("cdc manager: stopping runner (no longer in sources)", zap.String("svc", svc))
			r.Stop()
			delete(m.runners, svc)
		}
	}

	// 新增 / 重建
	for svc, src := range want {
		if old, exists := m.runners[svc]; exists {
			// 简化：先全停再重启（cofig 改动频率低）
			old.Stop()
		}
		r := NewRunner(src, m.publisher, m.schema, m.logger)
		m.runners[svc] = r
		m.wg.Add(1)
		go func(r *Runner, svc string) {
			defer m.wg.Done()
			if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				m.logger.Warn("cdc runner exited with error",
					zap.String("svc", svc), zap.Error(err))
			}
		}(r, svc)
		m.logger.Info("cdc manager: started runner", zap.String("svc", svc))
	}
}

// Stop 全部停掉并等 goroutines 退出。
func (m *Manager) Stop() {
	m.mu.Lock()
	for _, r := range m.runners {
		r.Stop()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Stat 给 admin web 看：每个服务的 runner 状态 + 上次位点。
type RunnerStat struct {
	Service     string `json:"service"`
	Running     bool   `json:"running"`
	LastFile    string `json:"last_file,omitempty"`
	LastPos     uint32 `json:"last_pos,omitempty"`
	LastGTID    string `json:"last_gtid,omitempty"`
}

// Stats 同步快照，给 /admin/cdc/status JSON 接口用。
func (m *Manager) Stats(ctx context.Context) []RunnerStat {
	m.mu.Lock()
	stats := make([]RunnerStat, 0, len(m.runners))
	for svc, r := range m.runners {
		file, pos, gtid, _ := r.publisher.LoadPosition(ctx, svc)
		stats = append(stats, RunnerStat{
			Service:  svc,
			Running:  !r.stopped,
			LastFile: file,
			LastPos:  pos,
			LastGTID: gtid,
		})
	}
	m.mu.Unlock()
	return stats
}

// errCanalNotWired 占位错误。等真接 canal 时删。
var errCanalNotWired = fmt.Errorf("canal binlog client not yet wired")
