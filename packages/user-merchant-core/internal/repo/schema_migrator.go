// schema_migrator.go — 启动期自愈 schema：跨 10 个分库给业务分片表建 _shadow 副本。
//
// 跟 order-core 的 ApplyShadowTables 同形（CREATE TABLE LIKE 主表）。生产路径：
//   1) generate.sh 产 init/${db}_init.sql      → 建主表
//   2) generate.sh 产 init/${db}_init_shadow.sql → 建影子表（新部署）
//   3) ApplyShadowTables 启动期补建（升级 / 漏 init 的兜底）
package repo

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// shardTableBases 跟 database/userdb/templates/schema.sql 的 11 张分片表对齐。
// 新增表时同步加这里，否则 ApplyShadowTables 不会建对应 _shadow。
var shardTableBases = []string{
	"users",
	"user_profiles",
	"user_auths",
	"login_logs",
	"user_sessions",
	"user_roles",
	"user_accounts",
	"user_settings",
	"merchants",
	"merchant_kyc_document",
	"merchant_channel_secret",
}

// ApplyShadowTables 给每张业务分片表建 _shadow 副本。
//
//	CREATE TABLE IF NOT EXISTS users_42_shadow LIKE users_42
//
// 行为：
//   - 主表不存在 → skip（init 流程未跑完 / 该业务表不在本 shard）
//   - 影子表已存在 → skip
//   - 单表错误不阻断其他表（合规启动期失败不让整服务挂）
//
// 调用：cmd/server/main.go OnStart 阶段，跟 idgen 注册一同跑。
func ApplyShadowTables(ctx context.Context, mgr *Manager, tablePerDB int, logger *zap.Logger) error {
	if mgr.ShardCount() == 0 {
		// 单库 dev 模式无 shard，直接跳过。
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("shadow-migrator")

	created, skipped, failed := 0, 0, 0
	start := time.Now()
	shards := mgr.AllShards()
	for shardIdx, db := range shards {
		dbName := fmt.Sprintf("user_merchant_db_%d", shardIdx)
		for tIdx := 0; tIdx < tablePerDB; tIdx++ {
			global := shardIdx*tablePerDB + tIdx
			suffix := fmt.Sprintf("%02d", global)
			for _, base := range shardTableBases {
				main := fmt.Sprintf("%s_%s", base, suffix)
				shadowTbl := main + "_shadow"

				exists, err := tableExists(ctx, db, dbName, main)
				if err != nil {
					failed++
					logger.Warn("introspect main failed",
						zap.String("db", dbName), zap.String("tbl", main), zap.Error(err))
					continue
				}
				if !exists {
					skipped++
					continue
				}
				ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` LIKE `%s`", shadowTbl, main)
				if err := db.WithContext(ctx).Exec(ddl).Error; err != nil {
					failed++
					logger.Warn("create LIKE failed",
						zap.String("db", dbName), zap.String("tbl", shadowTbl), zap.Error(err))
					continue
				}
				created++
			}
		}
	}
	logger.Info("shadow tables ensured",
		zap.Int("created_or_existing", created),
		zap.Int("skipped_no_main", skipped),
		zap.Int("failed", failed),
		zap.Duration("duration", time.Since(start)))
	return nil
}

// tableExists INFORMATION_SCHEMA 查表是否存在（schema 内）。
func tableExists(ctx context.Context, db *gorm.DB, dbName, tbl string) (bool, error) {
	var n int
	err := db.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?",
		dbName, tbl,
	).Scan(&n).Error
	return n > 0, err
}
