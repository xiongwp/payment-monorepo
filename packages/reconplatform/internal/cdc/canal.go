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

// Run 阻塞直到 ctx 取消 / Stop / 任一 canal 出错。
//
// 一个 Source 对应 N 个 binlog channel（每分片一个），每个 channel 一个独立
// canal goroutine：
//   - 独立 server-id（避免 MySQL 拒连）
//   - 独立位点持久化（recon:cdc:pos:<svc>:<idx>）
//   - 独立 enricher / filter 共享（同 service 业务语义一致）
//
// 任一 channel goroutine 异常 → log warn，但不退出整个 Runner（其他 shard 继续）。
// 真正退出条件：ctx.Cancel 或 Stop()。
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return errors.New("runner: already stopped")
	}
	r.mu.Unlock()

	if err := r.source.Normalize(); err != nil {
		return fmt.Errorf("source normalize: %w", err)
	}
	channels := r.source.Channels()
	r.logger.Info("cdc runner: starting channels",
		zap.Int("channels", len(channels)),
		zap.Int("tables", len(r.source.Tables)))

	// 给所有 channel 共享同一个 handler 模板（enricher / filter 配置）；
	// 每个 channel clone 一份，独立持有位点状态。
	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(ch Channel) {
			defer wg.Done()
			r.runChannel(ctx, ch)
		}(ch)
	}

	// 等所有 channel 退出 / ctx 取消
	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()

	select {
	case <-ctx.Done():
		// 触发 stop 让所有 channel goroutine 退出
		r.Stop()
		<-doneCh
		return ctx.Err()
	case <-r.stopCh:
		<-doneCh
		return nil
	case <-doneCh:
		return errors.New("all canal channels exited")
	}
}

// runChannel 单个 binlog channel 的 goroutine：连 canal + 重连退避。
func (r *Runner) runChannel(ctx context.Context, ch Channel) {
	logger := r.logger.With(zap.String("addr", ch.Addr), zap.Uint32("server_id", ch.ServerID))

	// 单独 source（仅替换 ServerIDBase 让 newRealCanal 拿到对的 server_id）
	chSource := r.source
	chSource.ServerIDBase = ch.ServerID

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		default:
		}

		handler := newCanalHandler(chSource, r.publisher, r.schema, logger)
		// TODO: 暴露 enricher / filter 注册接口给 Manager，再传到这里
		c, err := newRealCanal(chSource, handler, ch.Addr)
		if err != nil {
			logger.Warn("canal init failed; retry", zap.Error(err), zap.Duration("backoff", backoff))
			if !sleepOrCancel(ctx, r.stopCh, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		r.mu.Lock()
		r.canal = c // 用最后一个 channel 的 canal 占位，给 Stop() 关掉所有时用
		r.mu.Unlock()

		logger.Info("canal channel started")
		err = c.Run() // 阻塞订阅
		c.Close()
		if err == nil {
			return
		}
		logger.Warn("canal channel exited with error; reconnect", zap.Error(err))
		if !sleepOrCancel(ctx, r.stopCh, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// sleepOrCancel：带 ctx + stopCh 的可中断 sleep；返 false 表示被中断，应退出。
func sleepOrCancel(ctx context.Context, stopCh chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-stopCh:
		return false
	}
}

// nextBackoff 指数退避，封顶 30s。
func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > 30*time.Second {
		return 30 * time.Second
	}
	return next
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
