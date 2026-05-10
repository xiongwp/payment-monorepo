// config_loader.go - 从 config-center 拉 cdc.sources / cdc.ttl + OnChange 热更新。
//
// 启动期：
//   1. configcenter.GetJSON("cdc.sources") → []Source
//   2. configcenter.GetJSON("cdc.ttl")     → map[string]string
//
// OnChange：
//   - sources 改了 → mgr.Reload(ctx, sources)（diff 增删 Runner）
//   - ttl 改了     → ttlProvider.Reload(rawMap)
//
// 没接到 config-center（dev / config-center 挂了）：fallback 用本仓
// configs/cdc.sources.yaml 默认配置；prod 模式下 config-center 不可达 fail-fast。
package cdc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// expandEnvShellLike 替代 os.ExpandEnv：兼容 bash 风格 ${VAR:-default}。
//
// os.ExpandEnv 只认 ${VAR}，遇到 ${VAR:-default} 会把整个 "VAR:-default" 当
// 变量名找，肯定找不到 → 替换成空字符串 → recon_cdc 用户密码全空 → 数据库
// access denied 1045。
//
// 本函数支持：
//
//	${VAR}              os.Getenv(VAR)（不存在返空）
//	${VAR:-default}     不存在或为空 → "default"，否则 os.Getenv(VAR)
//	$VAR                同 ${VAR}（保留 os.ExpandEnv 行为）
var bashEnvPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandEnvShellLike exported alias for cmd/recon-admin/main.go OnChange path.
// 之前 OnChange 用 os.ExpandEnv 不支持 ${VAR:-default}，导致 password 空 → 1045。
func ExpandEnvShellLike(s string) string { return expandEnvShellLike(s) }

func expandEnvShellLike(s string) string {
	// 先处理 ${VAR:-default} 形式，剩下的 $VAR / ${VAR} 交给 os.ExpandEnv
	s = bashEnvPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := bashEnvPattern.FindStringSubmatch(match)
		name := groups[1]
		def := groups[2] // 可能为空（无 :- 段）
		val := os.Getenv(name)
		if val == "" {
			return def
		}
		return val
	})
	return os.ExpandEnv(s)
}

// SourcesAndTTL 一份完整的 CDC 配置快照。
type SourcesAndTTL struct {
	Sources []Source           `yaml:"sources" json:"sources"`
	TTL     map[string]string  `yaml:"ttl"     json:"ttl"`
}

// LoadFromFile 从本地 YAML 文件读取（dev fallback）。
func LoadFromFile(path string) (*SourcesAndTTL, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	var out SourcesAndTTL
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("yaml parse: %w", err)
	}
	for i := range out.Sources {
		if err := out.Sources[i].Normalize(); err != nil {
			return nil, fmt.Errorf("source[%d] %q: %w", i, out.Sources[i].Service, err)
		}
		// env 替换 ${VAR} —— 简化：只对 password 做（其他字段一般是字面量）
		out.Sources[i].Password = expandEnvShellLike(out.Sources[i].Password)
		out.Sources[i].User = expandEnvShellLike(out.Sources[i].User)
	}
	return &out, nil
}

// LoadFromConfigCenterClient 给定一个能 GetJSON 的 client（configcenter.Client
// 或测试 mock），拉一份完整 sources+ttl。
//
// 我们刻意走 interface 不直接 import payment-util/configcenter，避免本包跟
// config-center SDK 强耦合（cdc 库下游有可能在没有 config-center 的环境用）。
type ConfigCenterClient interface {
	GetJSON(ctx context.Context, key string, out any) error
}

// LoadFromConfigCenter 拉 cdc.sources + cdc.ttl 拼成完整 config。
// 任何一个 key 缺失就返 error；caller 自己决定是否 fallback。
func LoadFromConfigCenter(ctx context.Context, cli ConfigCenterClient) (*SourcesAndTTL, error) {
	if cli == nil {
		return nil, fmt.Errorf("config-center client is nil")
	}
	out := &SourcesAndTTL{}

	// sources：业务 service / table / index 配置
	var sourceList struct {
		Sources []Source `json:"sources"`
	}
	if err := cli.GetJSON(ctx, "cdc.sources", &sourceList); err != nil {
		return nil, fmt.Errorf("get cdc.sources: %w", err)
	}
	out.Sources = sourceList.Sources

	// ttl：每表 / 每服务过期时间
	var ttlMap map[string]string
	if err := cli.GetJSON(ctx, "cdc.ttl", &ttlMap); err != nil {
		// ttl 可选；没配的话用全局默认
		ttlMap = map[string]string{"default": "30d"}
	}
	out.TTL = ttlMap

	for i := range out.Sources {
		if err := out.Sources[i].Normalize(); err != nil {
			return nil, fmt.Errorf("source[%d] %q: %w", i, out.Sources[i].Service, err)
		}
		out.Sources[i].Password = expandEnvShellLike(out.Sources[i].Password)
		out.Sources[i].User = expandEnvShellLike(out.Sources[i].User)
	}
	return out, nil
}

// IndexKeysOf 提取所有 source 的 index_columns 并集。
// meta.Syncer 用这个，把可用索引列的清单暴露给编辑器 autocomplete。
func IndexKeysOf(sources []Source) []string {
	seen := make(map[string]struct{})
	for _, s := range sources {
		for _, t := range s.Tables {
			for _, c := range t.IndexColumns {
				seen[c] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// MetaSourcesOf 把 cdc.Source 列表转成 meta 包用的 Source 列表（DSN 形式）。
// meta syncer 不订阅 binlog，只跑 SELECT information_schema，所以可以用第一个 addr。
func MetaSourcesOf(srcs []Source) []MetaSource {
	out := make([]MetaSource, 0, len(srcs))
	for _, s := range srcs {
		if len(s.Addrs) == 0 {
			continue
		}
		// DSN 拼接：root:pass@tcp(host:3306)/?parseTime=true
		// 用 SELECT 权限即可；CDC 用户 recon_cdc 已经有
		dsn := fmt.Sprintf("%s:%s@tcp(%s)/?parseTime=true&charset=utf8mb4",
			s.User, s.Password, s.Addrs[0])
		out = append(out, MetaSource{
			Service: s.Service,
			DSN:     dsn,
			Schemas: s.Schemas,
		})
	}
	return out
}

// MetaSource 跟 meta.Source 同形态；这里复制一份避免 cdc 反向 import meta。
// caller 自行转成 meta.Source。
type MetaSource struct {
	Service string
	DSN     string
	Schemas []string
}

// debugDump 给 admin 调试用：把当前配置 JSON 化。
func (c *SourcesAndTTL) DebugDump() string {
	b, _ := json.MarshalIndent(c, "", "  ")
	return string(b)
}
