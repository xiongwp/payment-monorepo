// fleet_cache.go — Fleet × Rotation 路由本地缓存（接入 config-center）
//
// 设计：
//   - 写侧少（一月一次轮换）：scheduler.swap / ensureProvisioned 完成后调
//     `Push(laID, subAccounts)` HTTP PUT 进 config-center
//   - 读侧多（每笔记账 5 leg × N TPS）：SDK 后台 watch namespace=accounting-system
//     自动把 fleet.{la_id} 的最新值推到本地 cache，atomic.Pointer 纳秒级读
//
// 替代之前的 ResolveFleetSubAccount 每次都查 DB（跨 shard SELECT）
//
// Key 格式：`fleet.{la_id}`（统一在 namespace=accounting-system 下）
// Value JSON：{"0":"608...", "1":"608...", ..., "99":"608..."}
//   key="0".."99" = sub_idx；value=当前 active sub-account_no
//
// Miss 兜底：Get miss 时 caller 应 fallback 到 DB（保证可用性，即 cache 还没就绪
// 或某 LA 刚 register 还没 push 时不会全功能瘫痪）。
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/xiongwp/payment-util/configcenter"
	"go.uber.org/zap"
)

// FleetCache fleet routing 本地缓存接口。
//
// 实现：configFleetCache（依赖 configcenter.Client 读 + HTTP PUT 写）
// 测试 / 退化：noopFleetCache（Get 永远 miss，Push 永远 nop）
type FleetCache interface {
	// Get fleet 中 user_id=subIdx 的当前 active account_no。
	// hit=false → caller 应 fallback 到 DB。
	Get(laID int64, subIdx int) (accountNo string, hit bool)

	// Push 把 LA 的 fleet active sub-account 列表写入 config-center。
	// subAccounts 是 [100]string；index=sub_idx；空字符串元素表示该 idx 无 active。
	// 调用时机：scheduler 完成 swap / ensureProvisioned 后。
	Push(ctx context.Context, laID int64, subAccounts []string) error
}

// configFleetCache 用 configcenter SDK 实现。
type configFleetCache struct {
	cli           *configcenter.Client
	configBaseURL string        // 写 API：直接 HTTP PUT 进 config-center server
	httpc         *http.Client  // 可空，用 default
	namespace     string        // "accounting-system"
	actor         string        // PUT 时 actor 字段（审计；如 hostname）
	logger        *zap.Logger
}

// NewFleetCache 构造。
//
// 入参：
//   - cli: 已 Init 的 configcenter SDK client（订阅 namespace=accounting-system，
//          已经在跑 watch；本服务直接读它的本地 cache）
//   - configBaseURL: config-center server 的 HTTP base URL（用于写）
//                    e.g. "http://config-center:9690"
//   - actor: PUT 审计字段（hostname / pod 名）
//
// cli 或 configBaseURL 为空 → 返回 noopFleetCache，Get 永远 miss，Push 永远成功
// （兼容老部署没接 config-center 的场景）。
func NewFleetCache(cli *configcenter.Client, configBaseURL, actor string, logger *zap.Logger) FleetCache {
	if logger == nil {
		logger = zap.NewNop()
	}
	if cli == nil || configBaseURL == "" {
		logger.Warn("fleet cache disabled (no configcenter client or baseURL); will always miss → DB fallback every leg",
			zap.Bool("client_nil", cli == nil), zap.String("baseURL", configBaseURL))
		return &noopFleetCache{}
	}
	if actor == "" {
		actor = "accounting-fleet-cache"
	}
	return &configFleetCache{
		cli:           cli,
		configBaseURL: configBaseURL,
		httpc:         &http.Client{Timeout: 5 * time.Second},
		namespace:     "accounting-system",
		actor:         actor,
		logger:        logger,
	}
}

// Get 实现 — 走 SDK 本地 cache（atomic.Pointer，纳秒级）。
func (c *configFleetCache) Get(laID int64, subIdx int) (string, bool) {
	if subIdx < 0 || subIdx >= 100 {
		return "", false
	}
	key := fleetKey(laID)
	// SDK GetString 内部走本地 cache；net 0 cost
	// JSON value 反序列化 — 用 map[string]string 解最简单
	v, err := c.cli.Get(context.Background(), key)
	if err != nil || v == nil {
		return "", false
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(v.Value), &m); err != nil {
		c.logger.Warn("fleet cache decode failed",
			zap.String("key", key), zap.Error(err))
		return "", false
	}
	accountNo, ok := m[strconv.Itoa(subIdx)]
	if !ok || accountNo == "" {
		return "", false
	}
	return accountNo, true
}

// Push 实现 — 直接 HTTP PUT 进 config-center server。
//
// 单 LA 一条 key，整体替换 value（不增量）。轮换不频繁（一个月一次），整体替换
// 是最简单也最一致的方式。
func (c *configFleetCache) Push(ctx context.Context, laID int64, subAccounts []string) error {
	if len(subAccounts) != 100 {
		return fmt.Errorf("fleet cache Push: expected 100 sub-accounts, got %d", len(subAccounts))
	}
	// 构造 {"0":"acc0", "1":"acc1", ..., "99":"acc99"}
	m := make(map[string]string, 100)
	for i, accNo := range subAccounts {
		if accNo == "" {
			continue // 允许某些 idx 暂时为空（部分 provision 失败时）
		}
		m[strconv.Itoa(i)] = accNo
	}
	valueJSON, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("fleet cache Push: marshal: %w", err)
	}

	// PUT /api/v1/configs/{ns}/{key}
	body := map[string]interface{}{
		"value":         string(valueJSON),
		"format":        "json",
		"actor":         c.actor,
		"change_reason": fmt.Sprintf("fleet sync after rotation: la_id=%d", laID),
	}
	bodyBytes, _ := json.Marshal(body)

	key := fleetKey(laID)
	u := fmt.Sprintf("%s/api/v1/configs/%s/%s", c.configBaseURL, c.namespace, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("fleet cache Push: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// config-center 审计：actor 必须同时放 HTTP header（X-Actor）+ body。
	// 单放 body 时 server 返 401 "X-Actor required"。
	req.Header.Set("X-Actor", c.actor)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("fleet cache Push: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("fleet cache Push: http %d: %s", resp.StatusCode, string(buf[:n]))
	}
	c.logger.Info("fleet cache pushed",
		zap.Int64("la_id", laID),
		zap.Int("sub_count", len(m)),
		zap.String("key", key))
	return nil
}

// fleetKey LA → config-center key 的统一映射。
//
// 跟其它 accounting-system 配置共享同一个 namespace（accounting-system），
// 用 "fleet." 前缀做命名隔离，避免跟 system_config 业务 key 撞名。
func fleetKey(laID int64) string {
	return fmt.Sprintf("fleet.%d", laID)
}

// ────────────────────────────────────────────────────────────────────
// noop 实现：未接入 config-center 的兼容退化
// ────────────────────────────────────────────────────────────────────

type noopFleetCache struct{}

func (n *noopFleetCache) Get(int64, int) (string, bool)               { return "", false }
func (n *noopFleetCache) Push(context.Context, int64, []string) error { return nil }
