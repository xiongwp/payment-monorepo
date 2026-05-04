package repo

import (
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// isDupKey 判断是否为 MySQL 唯一索引冲突。
func isDupKey(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		return true
	}
	return strings.Contains(err.Error(), "Duplicate entry")
}
