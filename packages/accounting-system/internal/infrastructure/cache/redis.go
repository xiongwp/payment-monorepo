package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RedisConfig Redis 连接配置
// 支持两种模式：
//   - Standalone: 单节点（开发/测试）
//   - Sentinel:   哨兵模式（生产，3节点 master+2replica，自动故障转移）
type RedisConfig struct {
	Mode           string   `mapstructure:"mode"`             // "standalone" | "sentinel"
	Addrs          []string `mapstructure:"addrs"`            // standalone: ["host:port"]; sentinel: [sentinel1, sentinel2, sentinel3]
	MasterName     string   `mapstructure:"master_name"`      // sentinel 模式主节点名称
	Password       string   `mapstructure:"password"`
	DB             int      `mapstructure:"db"`
	PoolSize       int      `mapstructure:"pool_size"`        // 连接池大小（建议 = workers数量）
	MinIdleConns   int      `mapstructure:"min_idle_conns"`
	DialTimeout    int      `mapstructure:"dial_timeout_ms"`  // ms
	ReadTimeout    int      `mapstructure:"read_timeout_ms"`
	WriteTimeout   int      `mapstructure:"write_timeout_ms"`
}

// NewRedisClient 根据配置创建 Redis 客户端
func NewRedisClient(cfg RedisConfig, logger *zap.Logger) (redis.UniversalClient, error) {
	dialTimeout := time.Duration(cfg.DialTimeout) * time.Millisecond
	readTimeout := time.Duration(cfg.ReadTimeout) * time.Millisecond
	writeTimeout := time.Duration(cfg.WriteTimeout) * time.Millisecond
	if dialTimeout == 0 {
		dialTimeout = 3 * time.Second
	}
	if readTimeout == 0 {
		readTimeout = 2 * time.Second
	}
	if writeTimeout == 0 {
		writeTimeout = 2 * time.Second
	}
	poolSize := cfg.PoolSize
	if poolSize == 0 {
		poolSize = 200
	}
	minIdle := cfg.MinIdleConns
	if minIdle == 0 {
		minIdle = poolSize
	}

	var client redis.UniversalClient
	if cfg.Mode == "sentinel" {
		if len(cfg.Addrs) == 0 || cfg.MasterName == "" {
			return nil, fmt.Errorf("sentinel mode requires addrs and master_name")
		}
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.MasterName,
			SentinelAddrs: cfg.Addrs,
			Password:      cfg.Password,
			DB:            cfg.DB,
			PoolSize:      poolSize,
			MinIdleConns:  minIdle,
			DialTimeout:   dialTimeout,
			ReadTimeout:   readTimeout,
			WriteTimeout:  writeTimeout,
		})
		logger.Info("redis sentinel client created",
			zap.String("masterName", cfg.MasterName),
			zap.Strings("sentinels", cfg.Addrs))
	} else {
		addr := "127.0.0.1:6379"
		if len(cfg.Addrs) > 0 {
			addr = cfg.Addrs[0]
		}
		client = redis.NewClient(&redis.Options{
			Addr:         addr,
			Password:     cfg.Password,
			DB:           cfg.DB,
			PoolSize:     poolSize,
			MinIdleConns: minIdle,
			DialTimeout:  dialTimeout,
			ReadTimeout:  readTimeout,
			WriteTimeout: writeTimeout,
		})
		logger.Info("redis standalone client created", zap.String("addr", addr))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}
	return client, nil
}
