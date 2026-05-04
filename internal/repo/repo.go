package repo

import "github.com/xiongwp/user-merchant-core/pkg/dbx"

// isDupKey 沿用 pkg/dbx 的实现，仓库内部继续用小写名保持 idiomatic。
func isDupKey(err error) bool { return dbx.IsDupKey(err) }
