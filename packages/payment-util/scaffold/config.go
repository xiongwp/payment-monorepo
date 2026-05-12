// config.go — 统一配置加载.
//
// 优先级 (高到低): env > yaml file > struct defaults.
// 不引 viper (避免外部依赖); stdlib + yaml 实现.

package scaffold

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 基础配置 — 所有 service 共用字段.
//
// 各 service 自己额外 struct 嵌它:
//
//   type Config struct {
//       scaffold.Config
//       MyServiceCustom string `yaml:"my_custom"`
//   }
type Config struct {
	ServiceName     string        `yaml:"service_name"`
	Addr            string        `yaml:"addr"`             // ":8091"
	LogLevel        string        `yaml:"log_level"`         // debug / info / warn / error
	LogDev          bool          `yaml:"log_dev"`           // true = human-readable
	AdminToken      string        `yaml:"admin_token"`       // /admin/* 鉴权
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`  // 默认 10s

	DB     DBConfig     `yaml:"db"`
	OAuth2 OAuth2Config `yaml:"oauth2"`
	Audit  AuditConfig  `yaml:"audit"`
	Rate   RateConfig   `yaml:"rate"`
	OTel   OTelConfig   `yaml:"otel"`
}

type DBConfig struct {
	DSN              string        `yaml:"dsn"`
	MaxOpenConns     int           `yaml:"max_open_conns"`
	MaxIdleConns     int           `yaml:"max_idle_conns"`
	ConnMaxLifetime  time.Duration `yaml:"conn_max_lifetime"`
	SlowThreshold    time.Duration `yaml:"slow_threshold"`    // GORM slow log
	AutoMigrate      bool          `yaml:"auto_migrate"`      // dev=true, prod=false
}

type OAuth2Config struct {
	IntrospectURL string `yaml:"introspect_url"` // oauth2-server /introspect
	Required      bool   `yaml:"required"`        // false = 中间件登记 client_id 但不阻拦
}

type AuditConfig struct {
	BaseURL string `yaml:"base_url"`
	Token   string `yaml:"token"`
}

type RateConfig struct {
	Enabled    bool `yaml:"enabled"`
	GlobalRPS  int  `yaml:"global_rps"`
	PerKeyRPS  int  `yaml:"per_key_rps"`
}

// applyDefaults 缺省值填充
func (c *Config) applyDefaults() {
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 10 * time.Second
	}
	if c.DB.MaxOpenConns == 0 {
		c.DB.MaxOpenConns = 30
	}
	if c.DB.MaxIdleConns == 0 {
		c.DB.MaxIdleConns = 10
	}
	if c.DB.ConnMaxLifetime == 0 {
		c.DB.ConnMaxLifetime = 30 * time.Minute
	}
	if c.DB.SlowThreshold == 0 {
		c.DB.SlowThreshold = 200 * time.Millisecond
	}
}

// loadEnv 用 env 覆盖 — 命名规则 {SERVICE}_FIELD_SUBFIELD.
// 例: DATA_RIGHTS_ADDR / DATA_RIGHTS_DB_DSN / DATA_RIGHTS_LOG_LEVEL.
//
// 也支持通用 fallback (不带 service 前缀): ADDR / LOG_LEVEL / DB_DSN ...
func (c *Config) loadEnv(serviceName string) {
	prefix := strings.ToUpper(strings.ReplaceAll(serviceName, "-", "_")) + "_"

	// 工具: 优先读 prefix 再 fallback 通用
	get := func(key string) string {
		if v := os.Getenv(prefix + key); v != "" {
			return v
		}
		return os.Getenv(key)
	}

	if v := get("ADDR"); v != "" {
		c.Addr = v
	}
	if v := get("LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := get("LOG_DEV"); v != "" {
		c.LogDev = v == "1" || strings.EqualFold(v, "true")
	}
	if v := get("ADMIN_TOKEN"); v != "" {
		c.AdminToken = v
	}
	if v := get("DB_DSN"); v != "" {
		c.DB.DSN = v
	}
	if v := get("DB_AUTO_MIGRATE"); v != "" {
		c.DB.AutoMigrate = v == "1" || strings.EqualFold(v, "true")
	}
	if v := get("OAUTH2_INTROSPECT_URL"); v != "" {
		c.OAuth2.IntrospectURL = v
	}
	if v := get("OAUTH2_REQUIRED"); v != "" {
		c.OAuth2.Required = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("AUDITLOG_URL"); v != "" { // audit 一般用通用名
		c.Audit.BaseURL = v
	}
	if v := os.Getenv("AUDITLOG_TOKEN"); v != "" {
		c.Audit.Token = v
	}
	if v := get("RATE_ENABLED"); v != "" {
		c.Rate.Enabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v := get("RATE_GLOBAL_RPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Rate.GlobalRPS = n
		}
	}
	if v := get("RATE_PER_KEY_RPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Rate.PerKeyRPS = n
		}
	}
}

// LoadConfig 全流程: file (optional) → env override → defaults.
// 各 service 把它包成自己的 LoadConfig() 加业务字段处理.
func LoadConfig(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		if err := loadYAMLFile(path, c); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	c.loadEnv(c.ServiceName)
	c.applyDefaults()
	return c, nil
}
