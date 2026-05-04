// Package repo 用 pkg/dbx 的 Manager；本文件只做一层类型别名，避免调用方一次性
// 大改。新代码可以直接 import pkg/dbx。
package repo

import (
	"github.com/xiongwp/user-merchant-core/pkg/dbx"
)

// DBConfig re-exported for backwards-compat.
type DBConfig = dbx.DBConfig

// Manager re-exported.
type Manager = dbx.Manager

// NewManager re-exported.
var NewManager = dbx.NewManager

// NewShardedManager re-exported.
var NewShardedManager = dbx.NewShardedManager

// SetSQLLogger re-exported.
var SetSQLLogger = dbx.SetSQLLogger
