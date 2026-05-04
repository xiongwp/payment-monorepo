package logging

// GormLogger 将 GORM 日志路由到分层日志系统。
//
// 路由规则：
//   - 普通 SQL 执行（含行数）→ repository.log（Info 级别）
//   - 慢查询（超过 slowQueryMs）→ repository.log（Warn）+ performance.log（Info，供监控/告警）
//   - GORM 框架错误（非 record not found）→ repository.log（Error）
//   - record not found → repository.log（Debug，业务正常情况）
//
// GORM 的 logger.Interface 要求实现四个方法：
//   LogMode / Info / Warn / Error / Trace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// GormLogger 实现 gorm.io/gorm/logger.Interface
type GormLogger struct {
	repoLogger  *zap.Logger
	perfLogger  *zap.Logger
	slowQueryMs int64 // 慢查询阈值（毫秒）
	level       gormlogger.LogLevel
}

// NewGormLogger 创建 GormLogger。
//
//	repoLogger  — GORM 普通/错误日志的目标（repository.log）
//	perfLogger  — 慢查询额外写入目标（performance.log）
//	slowQueryMs — 慢查询阈值（毫秒），0 表示使用默认值 200ms
func NewGormLogger(repoLogger, perfLogger *zap.Logger, slowQueryMs int64) *GormLogger {
	if slowQueryMs <= 0 {
		slowQueryMs = 200
	}
	return &GormLogger{
		repoLogger:  repoLogger,
		perfLogger:  perfLogger,
		slowQueryMs: slowQueryMs,
		level:       gormlogger.Info,
	}
}

// LogMode 实现 gormlogger.Interface.LogMode
func (l *GormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	nl := *l
	nl.level = level
	return &nl
}

// Info 实现 gormlogger.Interface.Info（GORM 框架信息消息）
func (l *GormLogger) Info(_ context.Context, msg string, args ...interface{}) {
	if l.level >= gormlogger.Info {
		l.repoLogger.Info(fmt.Sprintf(msg, args...))
	}
}

// Warn 实现 gormlogger.Interface.Warn（GORM 框架警告消息）
func (l *GormLogger) Warn(_ context.Context, msg string, args ...interface{}) {
	if l.level >= gormlogger.Warn {
		l.repoLogger.Warn(fmt.Sprintf(msg, args...))
	}
}

// Error 实现 gormlogger.Interface.Error（GORM 框架错误消息）
func (l *GormLogger) Error(_ context.Context, msg string, args ...interface{}) {
	if l.level >= gormlogger.Error {
		l.repoLogger.Error(fmt.Sprintf(msg, args...))
	}
}

// Trace 实现 gormlogger.Interface.Trace（每条 SQL 执行后回调，含耗时和影响行数）
//
// 日志策略（按热路径性能反向调优）：
//   - 真实错误（非 record not found）→ repository.log Error
//   - 慢查询（> slowQueryMs）         → repository.log Warn + performance.log Info
//   - record not found                 → repository.log Debug（业务常态，避免噪音）
//   - 普通快查询                       → 完全不记日志（性能关键决策）
//
// 历史教训：之前每条快查询都同时写 repository.log + performance.log Info，
// 在 DoubleEntryBooking 这种每请求 5-10 个 SQL 的热路径下会产生：
//   - 每条 ~200B JSON × 双写 × 高频 = 数 GB/小时 日志 I/O
//   - zap field allocation × 每条 ~10 个 String/Float64 → GC 压力
//   - performance.log slow=true 标签依赖能反向查询，但完全可以用 percentiles
//     histograms 取代，无需逐条写文件
//
// 如需短期 debug 全部 SQL，调 LogMode(gormlogger.Info) 后再加 explicit branch。
func (l *GormLogger) Trace(_ context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	isSlow := elapsed > time.Duration(l.slowQueryMs)*time.Millisecond
	isRealErr := err != nil && !errors.Is(err, gorm.ErrRecordNotFound)

	// Fast-path: 普通快查询 + 无错误 → 直接返回，零分配
	if !isSlow && !isRealErr {
		return
	}

	// 只在需要时调 fc()（fc 内部会拼 SQL，是 GORM 中相对昂贵的操作）
	sql, rows := fc()
	elapsedMs := float64(elapsed.Microseconds()) / 1000.0

	switch {
	case isRealErr && l.level >= gormlogger.Error:
		l.repoLogger.Error("gorm error",
			zap.String("sql", sql),
			zap.Int64("rows", rows),
			zap.Float64("duration_ms", elapsedMs),
			zap.Error(err),
		)
	case isSlow && l.level >= gormlogger.Warn:
		l.repoLogger.Warn("gorm slow query",
			zap.String("sql", sql),
			zap.Int64("rows", rows),
			zap.Float64("duration_ms", elapsedMs),
		)
		l.perfLogger.Info("db_slow",
			zap.String("sql", sql),
			zap.Float64("duration_ms", elapsedMs),
			zap.Int64("rows", rows),
		)
	}
}
