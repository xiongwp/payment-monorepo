// manager.go — External Importer 调度 + 配置加载。
//
// 启动期从 config-center key=reconplatform/external.sources（YAML/JSON）拉
// sources，按 schedule 注册 cron，到点自动跑 Importer.Run。
//
// 也支持 admin web POST /api/v1/external/run/{name} 立即触发一次（不等 cron）。

package external

import (
	"context"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// Manager 一组 external sources 的运行时。
type Manager struct {
	rdb     redis.UniversalClient
	log     *zap.Logger
	mu      sync.Mutex
	imps    map[string]*Importer
	cron    *cron.Cron
	cronIDs map[string]cron.EntryID
}

func NewManager(rdb redis.UniversalClient, log *zap.Logger) *Manager {
	if log == nil {
		log = zap.NewNop()
	}
	return &Manager{
		rdb:     rdb,
		log:     log,
		imps:    map[string]*Importer{},
		cron:    cron.New(),
		cronIDs: map[string]cron.EntryID{},
	}
}

// Register 注册一个外部源 + 立即按 schedule 排上 cron。
func (m *Manager) Register(src Source, t Transport, p Parser) error {
	if src.Name == "" {
		return fmt.Errorf("source name required")
	}
	im := NewImporter(src, t, p, m.rdb, m.log)
	m.mu.Lock()
	defer m.mu.Unlock()
	// 已存在的 source 替换：先删 cron entry
	if old, ok := m.cronIDs[src.Name]; ok {
		m.cron.Remove(old)
	}
	m.imps[src.Name] = im
	if src.Schedule == "" {
		m.log.Info("external source registered (no schedule)",
			zap.String("name", src.Name))
		return nil
	}
	id, err := m.cron.AddFunc(src.Schedule, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*60*1e9) // 30min
		defer cancel()
		if n, err := im.Run(ctx); err != nil {
			m.log.Warn("external import failed",
				zap.String("source", src.Name),
				zap.Int("partial_rows", n),
				zap.Error(err))
		}
	})
	if err != nil {
		return fmt.Errorf("cron schedule %q: %w", src.Schedule, err)
	}
	m.cronIDs[src.Name] = id
	m.log.Info("external source scheduled",
		zap.String("name", src.Name),
		zap.String("schedule", src.Schedule))
	return nil
}

// RunNow 即时触发一次（不等 cron）；admin web 端点用。
func (m *Manager) RunNow(ctx context.Context, name string) (int, error) {
	m.mu.Lock()
	im, ok := m.imps[name]
	m.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("source %q not registered", name)
	}
	return im.Run(ctx)
}

// List 返当前所有 sources（admin web 列表用）。
func (m *Manager) List() []Source {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Source, 0, len(m.imps))
	for _, im := range m.imps {
		out = append(out, im.src)
	}
	return out
}

// Start 启动 cron。
func (m *Manager) Start() { m.cron.Start() }

// Stop 等当前 import 跑完再退（最多 30s）。
func (m *Manager) Stop(ctx context.Context) {
	stopped := m.cron.Stop()
	select {
	case <-stopped.Done():
	case <-ctx.Done():
	}
}

// BuildTransport 工厂：给 Source 配置里的 transport 字典造 Transport 对象。
//
// 支持 type=local|http|sftp 三种。未来加 s3/gcs 在这里 switch。
func BuildTransport(cfg map[string]any) (Transport, error) {
	typ, _ := cfg["type"].(string)
	switch typ {
	case "local", "":
		base, _ := cfg["path"].(string)
		return LocalTransport{BasePath: base}, nil
	case "http", "https":
		return HTTPTransport{
			URL:    asString(cfg["url"]),
			Bearer: asString(cfg["bearer"]),
		}, nil
	case "sftp":
		return SFTPTransport{
			Host:        asString(cfg["host"]),
			Port:        asInt(cfg["port"]),
			User:        asString(cfg["user"]),
			Password:    asString(cfg["password"]),
			IdentityKey: asString(cfg["identity_key"]),
			BasePath:    asString(cfg["path"]),
			StrictHost:  asBool(cfg["strict_host"]),
		}, nil
	}
	return nil, fmt.Errorf("unknown transport type %q", typ)
}

// BuildParser 工厂；支持 csv / mt940 / camt053 (银行流水) 三类.
//
// MT940: SWIFT 银行结算文件
// CAMT.053: ISO 20022 XML, SEPA 主流, 企业 Treasury 必备
func BuildParser(cfg map[string]any) (Parser, error) {
	typ, _ := cfg["type"].(string)
	switch typ {
	case "csv", "":
		delim := ','
		if d := asString(cfg["delimiter"]); d != "" {
			delim = []rune(d)[0]
		}
		header, ok := cfg["header"].(bool)
		if !ok {
			header = true
		}
		return CSVParser{
			Delimiter: delim,
			HasHeader: header,
			Timezone:  asString(cfg["timezone"]),
		}, nil
	case "mt940":
		return NewMT940Parser(), nil
	case "camt053", "camt.053", "iso20022":
		return NewCAMT053Parser(), nil
	}
	return nil, fmt.Errorf("unknown parser type %q", typ)
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
