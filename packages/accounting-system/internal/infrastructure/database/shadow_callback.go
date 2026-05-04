// Package database — shadow_callback 在 GORM 生成 SQL 前重写表名。
//
// 设计动机：accounting-system 22 个 repository 用了 struct TableName /
// router.GetTableName / 字面量 Table(...) 三种表名拼接方式。要让全部访问点
// 都按 ctx.IsShadow 走 *_shadow 表，最干净的办法是在 GORM 框架层统一拦截。
//
// 工作原理：注册到 Query / Create / Update / Delete / Row / Raw 6 类 callback
// 的 "before" 阶段；每次执行前检查 stmt.Context 的 shadow flag，若 true 且
// 当前 stmt.Table 不是 _shadow 后缀，则改写为 stmt.Table + "_shadow"。
//
// 限制：
//   - 已经以 _shadow 结尾的表名不重复加（防止 stmt 被多次执行的偏门 case）
//   - Raw 字符串里硬编码的表名 callback 看不到 — 业务侧仍需自己确保 Raw SQL
//     不在 shadow 路径调用，或显式包 shadow.TableName(ctx, base)
//   - Joins / 子查询里的表名是 SQL 字符串的一部分；如有跨表关联 shadow，
//     repo 层须自己拼 shadow 后缀
package database

import (
	"strings"

	"github.com/xiongwp/payment-util/shadow"
	"gorm.io/gorm"
)

// shadowSuffix 与 payment-util/shadow.Suffix 等价；这里复制一份避免热路径跨包查 var。
const shadowSuffix = "_shadow"

const shadowCallbackName = "shadow:rewrite_table"

// registerShadowCallback 在 gorm DB 上注册 6 类 before-callback。
// gorm 1.x API 链式调用：db.Callback().Query().Before("gorm:query").Register(name, fn)
func registerShadowCallback(db *gorm.DB) error {
	cb := db.Callback()

	// 6 类标准 callback。gorm 内置的 hook 名见 gorm/callbacks.go：
	//   gorm:query / gorm:create / gorm:update / gorm:delete / gorm:row / gorm:raw
	pairs := []struct {
		register func(name string, fn func(*gorm.DB)) error
	}{
		{cb.Query().Before("gorm:query").Register},
		{cb.Create().Before("gorm:create").Register},
		{cb.Update().Before("gorm:update").Register},
		{cb.Delete().Before("gorm:delete").Register},
		{cb.Row().Before("gorm:row").Register},
		{cb.Raw().Before("gorm:raw").Register},
	}
	for _, p := range pairs {
		if err := p.register(shadowCallbackName, rewriteTableForShadow); err != nil {
			return err
		}
	}
	return nil
}

// rewriteTableForShadow 在执行 SQL 前根据 ctx.IsShadow 改写 stmt.Table。
//
// 表名优先从 stmt.Table 取（业务调过 Table(...) / Model 后由 schema 解析填上）；
// stmt.Table 还未解析出来时退化用 Schema.Table（gorm 反射主表名）。
func rewriteTableForShadow(db *gorm.DB) {
	if db.Statement == nil {
		return
	}
	ctx := db.Statement.Context
	if !shadow.IsShadow(ctx) {
		return
	}
	tbl := db.Statement.Table
	if tbl == "" && db.Statement.Schema != nil {
		// 没显式 Table 时用 schema 反射出的默认表名（即 struct.TableName() 返回值）
		tbl = db.Statement.Schema.Table
	}
	if tbl == "" || strings.HasSuffix(tbl, shadowSuffix) {
		return
	}
	db.Statement.Table = tbl + shadowSuffix
}
