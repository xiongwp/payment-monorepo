package repo

import (
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// IsDupKey 判断是否为 MySQL 唯一索引冲突（exported 给 service 层用，比如
// refund 幂等：INSERT 撞 uk_pi_idem 时调用方可知道走读已有 row 路径）。
func IsDupKey(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		return true
	}
	return strings.Contains(err.Error(), "Duplicate entry")
}

// isDupKey 内部别名，保留旧调用方
func isDupKey(err error) bool { return IsDupKey(err) }
