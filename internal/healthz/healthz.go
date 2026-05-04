// Package healthz — 本服务的 /healthz 探针实现。
//
// 目前只有一个：DBPinger 用 GORM 底层 *sql.DB.PingContext 确认 meta 库可达。
// 后续要加 KMS 探针 / cache 探针都在这里挂一个实现。
package healthz

import (
	"context"
	"fmt"
	"time"

	"github.com/xiongwp/user-merchant-core/pkg/dbx"
)

// DBPinger 用底层 sql.DB.Ping 快速探测 meta DB 可达。replica 不参与 —— 主库挂了
// 等于服务不可用，replica 临时掉了读路径会自动回退主库。
type DBPinger struct{ mgr *dbx.Manager }

// NewDBPinger 构造
func NewDBPinger(mgr *dbx.Manager) *DBPinger { return &DBPinger{mgr: mgr} }

// Name 在 /healthz body 里显示
func (p *DBPinger) Name() string { return "meta-db" }

// Ping 超时由上层 ctx 控制；内部额外套 300ms 兜底，防 PingContext 不响应 ctx。
func (p *DBPinger) Ping(ctx context.Context) error {
	sqlDB, err := p.mgr.GetMeta().DB()
	if err != nil {
		return fmt.Errorf("sql db handle: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	return sqlDB.PingContext(ctx)
}
