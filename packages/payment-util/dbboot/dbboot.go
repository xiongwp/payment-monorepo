// Package dbboot 已废弃 — 数据库初始化改用 docker-entrypoint-initdb.d 方案。
//
// 决策：MySQL 容器有内置机制 /docker-entrypoint-initdb.d/，把 .sql 文件挂进去，
// 容器**首次启动**（数据卷为空时）会按字典序自动执行所有脚本。这是行业标准，
// 比应用启动期 embed + 跑 SQL 更简单：
//
//   - 不依赖应用启动顺序（DB 先 ready，应用才连接）
//   - 重启容器（数据卷有数据）不会重跑，避免 INSERT IGNORE / IF NOT EXISTS 噪音
//   - 不需要应用 binary embed SQL，改 schema 不用重编译应用
//   - DBA / 运维操作清晰：看 docker-compose.yml 就知道首次会跑什么
//
// 各仓 docker-compose.yml 已挂载：
//
//	id-generator:        ./internal/database/init.sql + init_shadow.sql
//	user-merchant-core:  ./database/metadb/init/init.sql + init_shadow.sql
//	accounting-system:   ./database/metadb/init/init.sql + init_shadow.sql
//	                     ./database/accountingdb/init/[0-9]_init*.sql 全套
//
// 老库的 schema 演进（生产已有数据后加新表 / 加列）由各仓自己的 migrator
// 处理（参见 order-core/internal/repo/schema_migrator.go 的 ApplyMetaMigration
// + ApplyShadowTables 模式）。
//
// 本包保留为空 stub 让 git history 明确记录"曾经引入过 dbboot 但撤回"的
// 决策；下次 release 可整体删除目录。
package dbboot
