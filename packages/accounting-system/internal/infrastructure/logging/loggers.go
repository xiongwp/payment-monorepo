package logging

// Package logging 提供分层日志基础设施。
//
// 架构：
//   - app.log         — 全局兜底日志（向后兼容，不在任何层的日志均落此处）
//   - api.log         — gRPC API 层日志：每条 RPC 请求/响应均落此处
//   - service.log     — 业务逻辑层日志：核心记账、TCC、日切、批量任务处理逻辑
//   - repository.log  — 数据访问层日志：GORM 操作、分片路由、慢查询
//   - performance.log — 性能追踪日志：API 耗时、DB 查询耗时、异步任务处理耗时
//
// fx 集成（无需修改任何 Service/Repository 构造函数签名）：
//   main.go 通过 fx.Module + fx.Decorate 在不同层注入对应 *zap.Logger：
//     - repository 层 Module → logger.Repository
//     - service    层 Module → logger.Service
//     - api        层 Module → logger.API
//   顶层（TCC恢复 Worker、热路径 Worker 等）使用 logger.App（默认）。
//
// 日志均同时写入文件 + 标准输出（JSON 格式），方便本地调试和日志采集平台解析。

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// LogConfig 日志配置（从 viper "logging" key 读取）
type LogConfig struct {
	// Dir 日志文件目录。
	// 非空时同时写文件和 stdout（由 EnableStdout 控制）；
	// 空字符串（""）表示纯 stdout 模式，不写任何文件——推荐 Docker/K8s 环境使用，
	// 避免容器内文件权限问题，日志由平台统一采集 stdout/stderr。
	Dir string `mapstructure:"dir"`
	// Level 全局日志级别（debug/info/warn/error，默认 "info"）
	Level string `mapstructure:"level"`
	// SlowQueryMs GORM 慢查询阈值（毫秒），超过时在 performance.log 额外记录（默认 200）
	SlowQueryMs int64 `mapstructure:"slow_query_ms"`
	// EnableStdout 是否同时输出到标准输出（默认 true）；Dir 为空时此选项无效（始终写 stdout）
	EnableStdout bool `mapstructure:"enable_stdout"`
}

// Loggers 分层日志实例集合
type Loggers struct {
	// App 全局兜底日志（写入 app.log + stdout，供顶层组件使用）
	App *zap.Logger
	// API gRPC API 层日志（写入 api.log，记录请求/响应/耗时）
	API *zap.Logger
	// Service 业务逻辑层日志（写入 service.log，核心记账、TCC、日切逻辑）
	Service *zap.Logger
	// Repository 数据访问层日志（写入 repository.log，GORM、分片路由）
	Repository *zap.Logger
	// Performance 性能追踪日志（写入 performance.log，DB查询、API、任务耗时）
	Performance *zap.Logger
	// SlowQueryMs GORM 慢查询阈值（毫秒）
	SlowQueryMs int64
}

// NewLoggers 根据 viper 配置构建分层日志实例。
//
// 两种模式：
//   - 文件模式（Dir 非空）：日志同时写文件（Dir/xxx.log）和 stdout（由 EnableStdout 控制）
//   - 纯 stdout 模式（Dir 为空 ""）：不写任何文件，所有日志写 stdout，适合 Docker/K8s
func NewLoggers(v *viper.Viper) (*Loggers, error) {
	var cfg LogConfig
	_ = v.UnmarshalKey("logging", &cfg)

	if cfg.Level == "" {
		cfg.Level = "info"
	}
	if cfg.SlowQueryMs == 0 {
		cfg.SlowQueryMs = 200
	}

	level := parseLevel(cfg.Level)

	// 纯 stdout 模式：Dir 为空时不写文件，所有日志直接输出到 stdout。
	// 这是 Docker/K8s 的推荐做法：平台负责采集 stdout，无需容器内文件权限。
	if cfg.Dir == "" {
		makeStdout := func(layer string) *zap.Logger {
			return newStdoutLogger(layer, level)
		}
		return &Loggers{
			App:         makeStdout("app"),
			API:         makeStdout("api"),
			Service:     makeStdout("service"),
			Repository:  makeStdout("repository"),
			Performance: makeStdout("performance"),
			SlowQueryMs: cfg.SlowQueryMs,
		}, nil
	}

	// 文件模式：创建日志目录，各层日志写文件（同时可选 stdout）
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %s: %w", cfg.Dir, err)
	}

	make := func(filename, layer string) (*zap.Logger, error) {
		return newLayerLogger(cfg.Dir, filename, layer, level, cfg.EnableStdout)
	}

	app, err := make("app.log", "app")
	if err != nil {
		return nil, fmt.Errorf("app logger: %w", err)
	}
	api, err := make("api.log", "api")
	if err != nil {
		return nil, fmt.Errorf("api logger: %w", err)
	}
	svc, err := make("service.log", "service")
	if err != nil {
		return nil, fmt.Errorf("service logger: %w", err)
	}
	repo, err := make("repository.log", "repository")
	if err != nil {
		return nil, fmt.Errorf("repository logger: %w", err)
	}
	perf, err := make("performance.log", "performance")
	if err != nil {
		return nil, fmt.Errorf("performance logger: %w", err)
	}

	return &Loggers{
		App:         app,
		API:         api,
		Service:     svc,
		Repository:  repo,
		Performance: perf,
		SlowQueryMs: cfg.SlowQueryMs,
	}, nil
}

// newLayerLogger 创建单个层的 zap.Logger，写入指定文件，同时可选输出到 stdout。
// 每条日志携带固定字段 "layer"，方便在集中日志平台过滤。
func newLayerLogger(dir, filename, layer string, level zapcore.Level, enableStdout bool) (*zap.Logger, error) {
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "ts"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderCfg.EncodeLevel = zapcore.LowercaseLevelEncoder

	encoder := zapcore.NewJSONEncoder(encoderCfg)
	levelEnabler := zap.NewAtomicLevelAt(level)

	// 文件 Writer
	filePath := dir + "/" + filename
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filePath, err)
	}
	fileCore := zapcore.NewCore(encoder, zapcore.AddSync(f), levelEnabler)

	var core zapcore.Core
	if enableStdout {
		stdoutCore := zapcore.NewCore(encoder, zapcore.AddSync(os.Stdout), levelEnabler)
		core = zapcore.NewTee(fileCore, stdoutCore)
	} else {
		core = fileCore
	}

	logger := zap.New(core,
		zap.AddCaller(),
		zap.AddCallerSkip(0),
		// 固定字段：标识日志所属层，便于集中日志平台过滤
		zap.Fields(zap.String("layer", layer)),
	)
	return logger, nil
}

// newStdoutLogger 创建仅写 stdout 的 zap.Logger（纯 stdout 模式，适用于 Docker/K8s）。
func newStdoutLogger(layer string, level zapcore.Level) *zap.Logger {
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "ts"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderCfg.EncodeLevel = zapcore.LowercaseLevelEncoder

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.AddSync(os.Stdout),
		zap.NewAtomicLevelAt(level),
	)
	return zap.New(core,
		zap.AddCaller(),
		zap.Fields(zap.String("layer", layer)),
	)
}

// parseLevel 将字符串解析为 zapcore.Level（无效值回退到 info）
func parseLevel(s string) zapcore.Level {
	switch strings.ToLower(s) {
	case "debug":
		return zapcore.DebugLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}
