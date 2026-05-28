//go:build feast

// Package mlscore —— Feast online feature provider integration.
//
// 本文件是 ML 推理的"特征源升级"：把 mlscore.Features 从 mlFeaturesFrom(txn)
// 临时计算改成从 Feast online store 拉。两条路径并存：
//
//   - 默认 build（无 -tags=feast）：本文件不编译；老路径不动
//   - -tags=feast：FeastFeatureProvider 可用，main.go 按 cfg.Feast.Enabled
//                  挂到 mlscore.Service 主路径前
//
// fail-soft 设计：任一 entity 拉取失败 / 超时 → Provide 返 (zero, err)，
// 调用方 fallback 到 mlFeaturesFrom(txn) 老路径。不影响 Screen 主链路 SLA。
//
// 详见 deploy/feast/README.md 接入流程章节。
package mlscore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/xiongwp/risk-manage/internal/featurestore"
)

// FeatureProvider 从外部 feature store 拉一组 ML 推理用特征。返回 Features 部分
// 字段填充（velocity / device / ip / merchant 相关）；其它字段（Amount /
// Currency / fingerprint hash）由 caller 自己从 txn 拷入再合并。
//
// 为啥拆 Provider 而不是塞 Service：Service 是模型推理；Provider 是数据加载。
// 单测时 fake provider 注独立 feature 集合，跟模型实现解耦。
type FeatureProvider interface {
	Provide(ctx context.Context, customerID, deviceID, ip, merchantID string) (Features, error)
}

// NoopProvider 返 zero Features + nil。给禁用 Feast 时占位。
type NoopProvider struct{}

func (NoopProvider) Provide(_ context.Context, _, _, _, _ string) (Features, error) {
	return Features{}, nil
}

// FeastFeatureProvider 用 featurestore.FeastClient 从 Feast online store
// 并发拉 4 个 entity 的特征 → 拼成 Features。
type FeastFeatureProvider struct {
	cli     *featurestore.FeastClient
	timeout time.Duration // 单 entity 拉取超时；总耗时约等于 max(4 个 entity)
}

// NewFeastFeatureProvider 构造。timeout=0 时默认 50ms。
func NewFeastFeatureProvider(cli *featurestore.FeastClient, timeout time.Duration) *FeastFeatureProvider {
	if timeout <= 0 {
		timeout = 50 * time.Millisecond
	}
	return &FeastFeatureProvider{cli: cli, timeout: timeout}
}

// Provide 并发拉 4 个 entity 的特征，组装成 mlscore.Features。
//
// 错误聚合策略：任一 entity 报 ErrFeastTimeout / ErrFeastNotEnabled 整体
// 返 err，让调用方 fallback。部分 entity 成功 + 部分失败也算失败——保守，
// 避免推理用半残特征产出误导分数。
func (p *FeastFeatureProvider) Provide(ctx context.Context, customerID, deviceID, ip, merchantID string) (Features, error) {
	if p == nil || p.cli == nil {
		return Features{}, errors.New("mlscore: nil FeastFeatureProvider")
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	type lookupRes struct {
		entity string
		m      map[string]float64
		err    error
	}
	var wg sync.WaitGroup
	results := make(chan lookupRes, 4)

	// 4 个并发 fetch。每个 lookup 自己有 50ms timeout（FeastClient 内部）。
	fetch := func(name string, fn func() (map[string]float64, error)) {
		defer wg.Done()
		// 空 entity ID 直接跳过；不发请求省网络
		m, err := fn()
		results <- lookupRes{entity: name, m: m, err: err}
	}
	wg.Add(4)
	go fetch("customer", func() (map[string]float64, error) {
		if customerID == "" {
			return nil, nil
		}
		return p.cli.GetCustomerFeatures(ctx, customerID)
	})
	go fetch("device", func() (map[string]float64, error) {
		if deviceID == "" {
			return nil, nil
		}
		return p.cli.GetDeviceFeatures(ctx, deviceID)
	})
	go fetch("ip", func() (map[string]float64, error) {
		if ip == "" {
			return nil, nil
		}
		return p.cli.GetIPFeatures(ctx, ip)
	})
	go fetch("merchant", func() (map[string]float64, error) {
		if merchantID == "" {
			return nil, nil
		}
		return p.cli.GetMerchantFeatures(ctx, merchantID)
	})
	wg.Wait()
	close(results)

	merged := map[string]float64{}
	var firstErr error
	for r := range results {
		if r.err != nil {
			// 软错误（NotEnabled / Timeout）转 hard err 让上层 fallback
			if firstErr == nil {
				firstErr = fmt.Errorf("feast %s: %w", r.entity, r.err)
			}
			continue
		}
		for k, v := range r.m {
			merged[k] = v
		}
	}
	if firstErr != nil {
		return Features{}, firstErr
	}

	// merged → Features。字段名跟 feature_views.py 一一对齐。
	feats := Features{
		MerchantID:           merchantID,
		CustomerID:           customerID,
		MouseSpeedVariance:   merged["mouse_speed_var"],
		KeystrokeDwellCV:     merged["keystroke_dwell_cv"],
		MousePauseCount:      int(merged["pause_count"]),
		IPProxy:              merged["is_proxy"] >= 0.5,
		IPVPN:                merged["is_vpn"] >= 0.5,
		Extra: map[string]string{
			// 高 cardinality / 字符串特征走 Extra；模型按 key 取
			"paid_count_90d":          strconv.FormatFloat(merged["paid_count_90d"], 'f', 0, 64),
			"chargeback_count_90d":    strconv.FormatFloat(merged["chargeback_count_90d"], 'f', 0, 64),
			"dispute_count_30d":       strconv.FormatFloat(merged["dispute_count_30d"], 'f', 0, 64),
			"first_seen_days":         strconv.FormatFloat(merged["first_seen_days"], 'f', 0, 64),
			"distinct_customers_90d":  strconv.FormatFloat(merged["distinct_customers_90d"], 'f', 0, 64),
			"fp_simhash_neighbors":    strconv.FormatFloat(merged["fp_simhash_neighbors"], 'f', 0, 64),
			"asn_score":               strconv.FormatFloat(merged["asn_score"], 'f', 3, 64),
			"merchant_30d_fraud_rate": strconv.FormatFloat(merged["merchant_30d_fraud_rate"], 'f', 4, 64),
			"merchant_avg_amount":     strconv.FormatFloat(merged["merchant_avg_amount"], 'f', 2, 64),
			"merchant_country_mix":    strconv.FormatFloat(merged["merchant_country_mix"], 'f', 3, 64),
		},
	}
	return feats, nil
}

// FeastConfig main.go 用的配置 struct。viper 解到这。
//
//   mlscore:
//     feast:
//       enabled: true
//       addr: "feast-server:6566"
//       project: "risk"
//       timeout_ms: 50
//       feature_service: "risk_realtime_v1"
type FeastConfig struct {
	Enabled        bool   `mapstructure:"enabled"`
	Addr           string `mapstructure:"addr"`
	Project        string `mapstructure:"project"`
	TimeoutMs      int    `mapstructure:"timeout_ms"`
	FeatureService string `mapstructure:"feature_service"`
}

// BuildProvider 工厂函数：cfg.Enabled && cfg.Addr 非空 → 返 FeastFeatureProvider；
// 否则返 NoopProvider 让调用方走 fallback。
//
// 失败 dial Feast 时返 NoopProvider + warn error（让 main.go 起服务但 log）。
func BuildProvider(cfg FeastConfig) (FeatureProvider, error) {
	if !cfg.Enabled || cfg.Addr == "" {
		return NoopProvider{}, nil
	}
	project := cfg.Project
	if project == "" {
		project = "risk"
	}
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	cli, err := featurestore.NewFeastClient(cfg.Addr, project, timeout)
	if err != nil {
		return NoopProvider{}, fmt.Errorf("feast dial %s: %w", cfg.Addr, err)
	}
	if cfg.FeatureService != "" {
		cli.FeatureService = cfg.FeatureService
	}
	return NewFeastFeatureProvider(cli, timeout), nil
}

// MergeFeatures 在保留 caller 已填字段（Amount / Currency 等）基础上，把
// FeastFeatureProvider 拉到的特征 overlay 进去。Feast 字段优先（更准、point-in-time）。
//
// 调用模式（在 service/risk.go 里）：
//
//	base := mlFeaturesFrom(txn)
//	if p != noop {
//	    fs, err := p.Provide(ctx, txn.CustomerID, ...)
//	    if err == nil {
//	        base = MergeFeatures(base, fs)
//	    }
//	    // err != nil 直接用 base（fallback）
//	}
//	res, _ := mlSvc.Score(ctx, base)
func MergeFeatures(base, overlay Features) Features {
	if overlay.MouseSpeedVariance != 0 {
		base.MouseSpeedVariance = overlay.MouseSpeedVariance
	}
	if overlay.KeystrokeDwellCV != 0 {
		base.KeystrokeDwellCV = overlay.KeystrokeDwellCV
	}
	if overlay.MousePauseCount != 0 {
		base.MousePauseCount = overlay.MousePauseCount
	}
	// Bool 字段：feast 拉到 = true 优先（IP 情报权威）
	if overlay.IPProxy {
		base.IPProxy = true
	}
	if overlay.IPVPN {
		base.IPVPN = true
	}
	// Extra 合并：feast key 优先
	if len(overlay.Extra) > 0 {
		if base.Extra == nil {
			base.Extra = map[string]string{}
		}
		for k, v := range overlay.Extra {
			base.Extra[k] = v
		}
	}
	return base
}
