// schema_migrator.go — 启动期自愈 schema：跨 10 个分库给业务分片表建 _shadow 副本，
// 给 paychan_meta 也建一份 leaf_alloc_shadow。
//
// 主路径：脚本 scripts/init-shared-db.sh 已经导入 init.sql + init_shadow.sql；
// 兜底路径：升级期或者漏 init 的环境下，应用启动时这里会调 CREATE TABLE LIKE 把
// 缺的 _shadow 表补上。两条路径都 idempotent。
package repo

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// shardTableBases 跟 database/paychandb/templates/schema.sql 的 4 张分片表对齐。
// 新增分片表时同步加这里 + database/paychandb/scripts/generate.sh 的 SHADOW_BASES。
var shardTableBases = []string{
	"acquirer_tx",
	"webhook_raw",
	"webhook_raw_rejected",
	"channel_token",
}

// metaShadowTables paychan_meta 库需要建影子副本的表清单。
// 与 database/metadb/init/init_shadow.sql 一一对应。
// leaf_alloc 也复制：让 shadow 流量从 leaf_alloc_shadow 取号，避免压测消耗主号段。
var metaShadowTables = []string{
	"leaf_alloc",
}

// ApplyMetaShadowTables 在 paychan_meta 上给每张需要影子的 meta 表创建 _shadow 副本。
// 启动期幂等兜底（init_shadow.sql 早跑过则全是 no-op）。失败按表计数，不阻断启动。
func ApplyMetaShadowTables(ctx context.Context, mgr *Manager, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("meta-shadow-migrator")
	meta := mgr.GetMeta()
	if meta == nil {
		return nil // 没单独 meta；GetMeta() 会 fallback 到 shard[0]，照常跑也行
	}
	created, failed := 0, 0
	start := time.Now()
	for _, base := range metaShadowTables {
		shadowTbl := base + "_shadow"
		ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` LIKE `%s`", shadowTbl, base)
		if err := meta.WithContext(ctx).Exec(ddl).Error; err != nil {
			failed++
			logger.Warn("create meta LIKE failed", zap.String("tbl", shadowTbl), zap.Error(err))
			continue
		}
		created++
	}
	logger.Info("meta shadow tables ensured",
		zap.Int("created_or_existing", created),
		zap.Int("failed", failed),
		zap.Duration("duration", time.Since(start)))
	return nil
}

// ApplyShadowTables 启动期跨 10 个分库给每张业务分片表建 _shadow 副本：
//
//	CREATE TABLE IF NOT EXISTS acquirer_tx_42_shadow LIKE acquirer_tx_42
//
// 行为：
//   - 主表不存在 → DDL 报错，记 warn 跳过下一个（init 流程未跑完 / 该表本部署不需要）
//   - 影子表已存在 → IF NOT EXISTS，no-op
//   - 单表错误不阻断其他表
//
// LIKE 复制结构（含索引），不复制 trigger / FK（业务表本就没用）。运行期 shadow
// 流量经 Router.TableName(ctx, ...) 直接落到 _shadow 表。
func ApplyShadowTables(ctx context.Context, mgr *Manager, tablePerDB int, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("shadow-migrator")

	created, failed := 0, 0
	start := time.Now()
	shards := mgr.AllShards()
	for shardIdx, db := range shards {
		for tIdx := 0; tIdx < tablePerDB; tIdx++ {
			global := shardIdx*tablePerDB + tIdx
			suffix := fmt.Sprintf("%02d", global)
			for _, base := range shardTableBases {
				main := fmt.Sprintf("%s_%s", base, suffix)
				shadowTbl := main + "_shadow"
				if err := createLike(ctx, db, shadowTbl, main); err != nil {
					failed++
					logger.Warn("create shard LIKE failed",
						zap.Int("shard", shardIdx),
						zap.String("tbl", shadowTbl),
						zap.Error(err))
					continue
				}
				created++
			}
		}
	}
	logger.Info("shadow tables ensured",
		zap.Int("created_or_existing", created),
		zap.Int("failed", failed),
		zap.Duration("duration", time.Since(start)))
	return nil
}

func createLike(ctx context.Context, db *gorm.DB, shadowTbl, mainTbl string) error {
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` LIKE `%s`", shadowTbl, mainTbl)
	return db.WithContext(ctx).Exec(ddl).Error
}
