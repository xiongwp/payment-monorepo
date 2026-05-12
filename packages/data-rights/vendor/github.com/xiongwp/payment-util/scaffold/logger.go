// logger.go — 统一 zap logger 构造.

package scaffold

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// NewLogger 根据 Config 构造 zap logger.
//   dev=true  → 人读 (开发友好)
//   dev=false → JSON (生产 / ELK 友好)
func NewLogger(cfg Config) *zap.Logger {
	var zcfg zap.Config
	if cfg.LogDev {
		zcfg = zap.NewDevelopmentConfig()
		zcfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		zcfg = zap.NewProductionConfig()
		zcfg.EncoderConfig.TimeKey = "ts"
		zcfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	}

	switch cfg.LogLevel {
	case "debug":
		zcfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "warn":
		zcfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		zcfg.Level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	default:
		zcfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}

	// 全局字段: service name 总在
	logger, err := zcfg.Build(zap.AddCaller(), zap.Fields(
		zap.String("service", cfg.ServiceName),
	))
	if err != nil {
		// fallback
		logger, _ = zap.NewProduction()
	}
	return logger
}
