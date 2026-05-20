// Package config — split-payment 统一配置加载.
//
// 跟 card-center / payment-core 风格一致 (viper + yaml + env override).
//
// 加载顺序:
//   1. config/config.yaml (默认值, 编译到镜像里)
//   2. $SPLIT_PAYMENT_CONFIG 指定的文件 (覆盖默认)
//   3. SPLIT_PAYMENT_* env 变量 (覆盖文件 — 给 docker-compose / k8s 用)
//
// 业务代码不再 envOr / envInt 散落, 全走 cfg.X.Y.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config split-payment 所有可调项.
//
// mapstructure tag = yaml key. viper 同时支持 env override:
//   server.grpc_port → SPLIT_PAYMENT_SERVER_GRPC_PORT
//   database.dsn     → SPLIT_PAYMENT_DATABASE_DSN
// (大写 + 下划线分隔 — viper SetEnvKeyReplacer 自动处理).
type Config struct {
	Env string `mapstructure:"env"`

	Log struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`

	Server struct {
		GRPCPort   int    `mapstructure:"grpc_port"`
		AdminToken string `mapstructure:"admin_token"`
	} `mapstructure:"server"`

	Admin struct {
		HTTPPort int `mapstructure:"http_port"`
	} `mapstructure:"admin"`

	MTLS struct {
		ServerCert string `mapstructure:"server_cert"`
		ServerKey  string `mapstructure:"server_key"`
		CACert     string `mapstructure:"ca_cert"`
	} `mapstructure:"mtls"`

	Database struct {
		DSN             string        `mapstructure:"dsn"`
		MaxOpenConns    int           `mapstructure:"max_open_conns"`
		MaxIdleConns    int           `mapstructure:"max_idle_conns"`
		ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	} `mapstructure:"database"`

	Accounting struct {
		GRPCAddr string `mapstructure:"grpc_addr"`
		HTTPURL  string `mapstructure:"http_url"`
	} `mapstructure:"accounting"`

	Registry struct {
		Endpoints []string `mapstructure:"endpoints"`
	} `mapstructure:"registry"`

	Kafka struct {
		Brokers     []string `mapstructure:"brokers"`
		EventTopic  string   `mapstructure:"event_topic"`
		AuditTopic  string   `mapstructure:"audit_topic"`
		Refund      struct {
			Topic    string `mapstructure:"topic"`
			DLQTopic string `mapstructure:"dlq_topic"`
			Group    string `mapstructure:"group"`
			MaxRetry int    `mapstructure:"max_retry"`
		} `mapstructure:"refund"`
	} `mapstructure:"kafka"`

	Risk struct {
		HTTPURL           string `mapstructure:"http_url"`
		AuthToken         string `mapstructure:"auth_token"`
		AMLHTTPURL        string `mapstructure:"aml_http_url"`
		AMLAuthToken      string `mapstructure:"aml_auth_token"`
		AMLThresholdMinor int64  `mapstructure:"aml_threshold_minor"`
		FailOpen          bool   `mapstructure:"fail_open"`
	} `mapstructure:"risk"`

	FX struct {
		HTTPURL   string `mapstructure:"http_url"`
		AuthToken string `mapstructure:"auth_token"`
	} `mapstructure:"fx"`

	Clearing struct {
		HTTPURL string `mapstructure:"http_url"`
	} `mapstructure:"clearing"`

	Saga struct {
		Enabled bool `mapstructure:"enabled"`
	} `mapstructure:"saga"`

	Workers struct {
		PayoutCron struct {
			LiveMode bool          `mapstructure:"live_mode"`
			Interval time.Duration `mapstructure:"interval"`
		} `mapstructure:"payout_cron"`
		HoldUnstick struct {
			Interval time.Duration `mapstructure:"interval"`
		} `mapstructure:"hold_unstick"`
		Reconcile struct {
			Interval time.Duration `mapstructure:"interval"`
		} `mapstructure:"reconcile"`
		Retry struct {
			Interval time.Duration `mapstructure:"interval"`
		} `mapstructure:"retry"`
	} `mapstructure:"workers"`

	Seed struct {
		GraphDir string `mapstructure:"graph_dir"`
	} `mapstructure:"seed"`

	OTel struct {
		ExporterOTLPEndpoint string `mapstructure:"exporter_otlp_endpoint"`
	} `mapstructure:"otel"`

	Cache struct {
		RedisAddr     string `mapstructure:"redis_addr"`
		RedisPassword string `mapstructure:"redis_password"`
		RedisDB       int    `mapstructure:"redis_db"`
	} `mapstructure:"cache"`
}

// Load 按"yaml → env override"顺序加载. configFile 空时走 SPLIT_PAYMENT_CONFIG
// env, 再空时走 ./config/config.yaml 默认路径.
func Load(configFile string) (*Config, error) {
	v := viper.New()

	// 1. 默认值
	setDefaults(v)

	// 2. 文件路径
	if configFile == "" {
		// 留 SPLIT_PAYMENT_CONFIG 给运维 override
		if env := getEnv("SPLIT_PAYMENT_CONFIG"); env != "" {
			configFile = env
		} else {
			v.AddConfigPath("./config")
			v.AddConfigPath("/etc/split-payment")
			v.SetConfigName("config")
			v.SetConfigType("yaml")
		}
	}
	if configFile != "" {
		v.SetConfigFile(configFile)
	}

	// 读文件 — 找不到不报错 (允许只靠 env / 默认值跑 dev)
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	// 3. env override
	v.SetEnvPrefix("SPLIT_PAYMENT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// 4. 兼容老 env 名 (历史包袱; 新 key 跟 SPLIT_PAYMENT_xxx 同步)
	bindLegacyEnv(v)

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("env", "dev")
	v.SetDefault("log.level", "info")
	v.SetDefault("server.grpc_port", 9098)
	v.SetDefault("admin.http_port", 9099)
	v.SetDefault("database.max_open_conns", 100)
	v.SetDefault("database.max_idle_conns", 20)
	v.SetDefault("database.conn_max_lifetime", 30*time.Minute)
	v.SetDefault("accounting.grpc_addr", "accounting-system:9091")
	v.SetDefault("kafka.event_topic", "split-payment.events")
	v.SetDefault("kafka.audit_topic", "split-payment.audit")
	v.SetDefault("kafka.refund.topic", "recon.refund.events")
	v.SetDefault("kafka.refund.group", "split-payment-refund-handler")
	v.SetDefault("kafka.refund.max_retry", 5)
	v.SetDefault("workers.payout_cron.interval", 1*time.Hour)
	v.SetDefault("workers.hold_unstick.interval", 1*time.Hour)
	v.SetDefault("workers.reconcile.interval", 24*time.Hour)
	v.SetDefault("workers.retry.interval", 30*time.Second)
	v.SetDefault("seed.graph_dir", "./examples/moneyflow-graphs")
}

// bindLegacyEnv 历史包袱: 老 env 名 (如 ACCOUNTING_GRPC_ADDR / SPLIT_GRPC_PORT / SPLIT_PAYMENT_DSN)
// 不符合 viper SetEnvPrefix("SPLIT_PAYMENT") 的形态 (前缀错 / 分隔符错), AutomaticEnv 抓不到.
// 这里显式绑定让旧 docker-compose 平滑迁移. 新 deployment 推荐用 SPLIT_PAYMENT_xxx 前缀; 老 env 留 6 个月后可删.
func bindLegacyEnv(v *viper.Viper) {
	pairs := map[string]string{
		// 外部基础设施 (无 SPLIT_PAYMENT_ 前缀, 通用 env 名跟其它服务共享):
		"accounting.grpc_addr":        "ACCOUNTING_GRPC_ADDR",
		"accounting.http_url":         "ACCOUNTING_HTTP_URL",
		"registry.endpoints":          "REGISTRY_ENDPOINTS",
		"risk.http_url":               "RISK_HTTP_URL",
		"risk.auth_token":             "RISK_AUTH_TOKEN",
		"risk.aml_http_url":           "AML_HTTP_URL",
		"risk.aml_auth_token":         "AML_AUTH_TOKEN",
		"fx.http_url":                 "FX_HTTP_URL",
		"fx.auth_token":               "FX_AUTH_TOKEN",
		"clearing.http_url":           "CLEARING_HTTP_URL",
		"mtls.server_cert":            "MTLS_SERVER_CERT",
		"mtls.server_key":             "MTLS_SERVER_KEY",
		"mtls.ca_cert":                "MTLS_CA_CERT",
		"otel.exporter_otlp_endpoint": "OTEL_EXPORTER_OTLP_ENDPOINT",
		"env":                         "RECON_ENV",

		// SPLIT_xxx (历史: 短前缀, 跟 SPLIT_PAYMENT_xxx 区分开):
		"server.grpc_port": "SPLIT_GRPC_PORT",
		"admin.http_port":  "SPLIT_ADMIN_HTTP_PORT",
		"seed.graph_dir":   "MONEYFLOW_SEED_DIR",

		// SPLIT_PAYMENT_xxx 历史无 . 分隔, AutomaticEnv 按 . → _ 转换抓不到, 显式绑定:
		"database.dsn":            "SPLIT_PAYMENT_DSN",
		"database.max_open_conns": "SPLIT_PAYMENT_DB_MAX_OPEN",
		"database.max_idle_conns": "SPLIT_PAYMENT_DB_MAX_IDLE",
		"saga.enabled":            "SPLIT_PAYMENT_SAGA",
		"workers.payout_cron.live_mode": "SPLIT_PAYMENT_PAYOUT_LIVE",
		"workers.payout_cron.interval":  "SPLIT_PAYMENT_PAYOUT_CRON_INTERVAL",
		"risk.aml_threshold_minor":      "SPLIT_PAYMENT_AML_THRESHOLD_CENTS",
		"risk.fail_open":                "SPLIT_PAYMENT_RISK_FAIL_OPEN",
		"kafka.brokers":                 "SPLIT_PAYMENT_KAFKA_BROKERS",
		"kafka.event_topic":             "SPLIT_PAYMENT_EVENT_TOPIC",
		"kafka.audit_topic":             "SPLIT_PAYMENT_AUDIT_TOPIC",
		"kafka.refund.topic":            "SPLIT_PAYMENT_REFUND_TOPIC",
		"kafka.refund.dlq_topic":        "SPLIT_PAYMENT_REFUND_DLQ_TOPIC",
		"kafka.refund.group":            "SPLIT_PAYMENT_REFUND_GROUP",
		"kafka.refund.max_retry":        "SPLIT_PAYMENT_REFUND_MAX_RETRY",
		"server.admin_token":            "SPLIT_PAYMENT_ADMIN_TOKEN",
	}
	for key, env := range pairs {
		_ = v.BindEnv(key, env)
	}
}

func getEnv(k string) string {
	// 单独抽出来给 Load 启动期用; 业务路径不再用.
	v := viper.New()
	v.SetEnvPrefix("")
	v.AutomaticEnv()
	return v.GetString(k)
}
