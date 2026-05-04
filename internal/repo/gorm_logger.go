package repo

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type ZapGormLogger struct {
	z             *zap.Logger
	SlowThreshold time.Duration
	LogLevel      gormlogger.LogLevel
	IgnoreRNF     bool
}

func NewZapGormLogger(z *zap.Logger) *ZapGormLogger {
	return &ZapGormLogger{
		z:             z,
		SlowThreshold: 200 * time.Millisecond,
		LogLevel:      gormlogger.Info,
		IgnoreRNF:     true,
	}
}

func (l *ZapGormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	c := *l
	c.LogLevel = level
	return &c
}

func (l *ZapGormLogger) Info(_ context.Context, msg string, args ...any) {
	if l.LogLevel >= gormlogger.Info {
		l.z.Sugar().Infof(msg, args...)
	}
}

func (l *ZapGormLogger) Warn(_ context.Context, msg string, args ...any) {
	if l.LogLevel >= gormlogger.Warn {
		l.z.Sugar().Warnf(msg, args...)
	}
}

func (l *ZapGormLogger) Error(_ context.Context, msg string, args ...any) {
	if l.LogLevel >= gormlogger.Error {
		l.z.Sugar().Errorf(msg, args...)
	}
}

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
