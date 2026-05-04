package repo

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// ZapGormLogger 将 gorm 的慢查询 / trace 日志桥接到 zap。
//
// 每个 repo 方法执行后的 SQL + 耗时都会通过此 logger 输出：
//
//	level=debug  sql="SELECT ... FROM payment_intent_15 WHERE ..."
//	             rows=1  elapsed_ms=3
//
// 阈值：
//   - err    任何 SQL 错误（除 RecordNotFound 默认降级 debug）
//   - slow   大于 SlowThreshold 时升到 warn
//   - 其它   debug
type ZapGormLogger struct {
	z             *zap.Logger
	SlowThreshold time.Duration
	LogLevel      gormlogger.LogLevel
	IgnoreRNF     bool // 忽略 gorm.ErrRecordNotFound
}

// NewZapGormLogger 构造
func NewZapGormLogger(z *zap.Logger) *ZapGormLogger {
	return &ZapGormLogger{
		z:             z,
		SlowThreshold: 200 * time.Millisecond,
		LogLevel:      gormlogger.Info,
		IgnoreRNF:     true,
	}
}

// LogMode 切换日志级别
func (l *ZapGormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	c := *l
	c.LogLevel = level
	return &c
}

// Info 常规信息
func (l *ZapGormLogger) Info(_ context.Context, msg string, args ...any) {
	if l.LogLevel >= gormlogger.Info {
		l.z.Sugar().Infof(msg, args...)
	}
}

// Warn 警告
func (l *ZapGormLogger) Warn(_ context.Context, msg string, args ...any) {
	if l.LogLevel >= gormlogger.Warn {
		l.z.Sugar().Warnf(msg, args...)
	}
}

// Error 错误
func (l *ZapGormLogger) Error(_ context.Context, msg string, args ...any) {
	if l.LogLevel >= gormlogger.Error {
		l.z.Sugar().Errorf(msg, args...)
	}
}

// Trace 每条 SQL 的详细 trace —— 这就是"repo 访问日志 + 执行时间"的落点
func (l *ZapGormLogger) Trace(_ context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)
	sql, rows := fc()

	fields := []zap.Field{
		zap.String("sql", sql),
		zap.Int64("rows", rows),
		zap.Int64("elapsed_ms", elapsed.Milliseconds()),
	}

	switch {
	case err != nil && !(l.IgnoreRNF && errors.Is(err, gorm.ErrRecordNotFound)):
		if l.LogLevel >= gormlogger.Error {
			l.z.Error("sql error", append(fields, zap.Error(err))...)
		}
	case l.SlowThreshold > 0 && elapsed > l.SlowThreshold:
		if l.LogLevel >= gormlogger.Warn {
			l.z.Warn("slow sql", fields...)
		}
	default:
		if l.LogLevel >= gormlogger.Info {
			l.z.Debug("sql", fields...)
		}
	}
}
