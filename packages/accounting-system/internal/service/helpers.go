package service

import (
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
)

const (
	mysqlErrDuplicateEntry uint16 = 1062
	// maxErrMsgLen 写入 DB 的 error_msg 最大长度，防止超大错误栈导致行膨胀
	maxErrMsgLen = 500
)

// isDuplicateKeyError 判断是否为 MySQL 唯一键冲突（error code 1062）
// 使用 MySQL driver 的结构化错误类型，比字符串匹配更可靠。
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == mysqlErrDuplicateEntry
	}
	// fallback：兼容被 fmt.Errorf("%w") 包裹多层的场景
	s := err.Error()
	return containsStr(s, "1062") || containsStr(s, "Duplicate entry")
}

// truncateErrMsg 截断错误信息到 maxErrMsgLen 字符，防止超大错误栈写入 DB
func truncateErrMsg(msg string) string {
	if len(msg) <= maxErrMsgLen {
		return msg
	}
	return msg[:maxErrMsgLen] + "…(truncated)"
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}
