//go:build feast

// Package featurestore —— Feast online feature client (build-tag gated).
//
// Feast (https://feast.dev) 是 Tecton 开源的 feature store 标准。架构两层：
//
//   - online store (Redis / DynamoDB)：低延迟 (<10ms) 单点 read，serving 用
//   - offline store (Parquet / BigQuery)：批量 join 出训练样本用
//
// 风控接 Feast 的核心动机：
//
//  1. **train-serve consistency**：训练样本和线上推理走同一份 transform
//     定义（feature view YAML），消除 train-serve skew
//  2. **统一特征发现**：feast UI / registry 让 DS 团队复用别人的特征
//  3. **point-in-time correctness**：offline join 自带防穿越（用 event_timestamp）
//
// 本文件只暴露 online client。offline 训练在 python 侧用 feast SDK 直接读，
// Go 服务不参与。
//
// ─── 通信协议 ───────────────────────────────────────────────
//
// Feast online store 暴露 gRPC：
//
//	service ServingService {
//	  rpc GetOnlineFeatures(GetOnlineFeaturesRequest) returns (GetOnlineFeaturesResponse);
//	}
//
//	request:
//	  feature_service: "risk_realtime_v1"            // 跟 ML 团队约定的 service 名
//	  entities: { "customer_id": [cust123, cust456] } // 批量 entity（省 RTT）
//	  full_feature_names: false
//
//	response:
//	  metadata.feature_names: [paid_count_90d, chargeback_count_90d, ...]
//	  results[i].values:      [typed_value, ...]                // 按 entity 顺序
//
// ─── 依赖管理 ────────────────────────────────────────────────
//
// Feast 官方没维护 Go SDK；标准做法是从 feast 仓库 .proto 自己 gen：
//
//	go install github.com/feast-dev/feast/sdk/go/protos/feast/serving@latest
//
// 真实 import path 由 ML 团队 vendor 后定（可能 v2 vs v0.x 不同）。本文件
// 用 interface 抽象 gRPC client，避免硬绑某个版本。**go.mod 暂不引**——
// build feast tag 打开时 ML 团队补依赖即可（构建会报错 missing import，明确暴露）。
package featurestore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ErrFeastNotEnabled —— stub 文件返这个；feast tag 编译时不会触发。
var ErrFeastNotEnabled = errors.New("featurestore: feast client not enabled (build without -tags=feast)")

// ErrFeastTimeout —— gRPC 超时；调用方应 fail-open。
var ErrFeastTimeout = errors.New("featurestore: feast online get timeout")

// servingClient 抽象 Feast gRPC stub。真实接入时换成：
//
//	servingClient = serving.NewServingServiceClient(conn)
//
// 这里用 interface 解耦 proto 版本。
type servingClient interface {
	GetOnlineFeatures(ctx context.Context, req *onlineRequest) (*onlineResponse, error)
}

// onlineRequest / onlineResponse 是 proto 类型的占位 shape。实际接入时
// 替换成 `serving.GetOnlineFeaturesRequest` / `GetOnlineFeaturesResponse`。
type onlineRequest struct {
	FeatureService string
	Project        string
	EntityRows     []map[string]string
	FeatureRefs    []string
}

type onlineResponse struct {
	FeatureNames []string
	// Values[i][j] = i 个 entity 的第 j 个 feature 的 float 值
	Values [][]float64
}

// FeastClient online feature lookup client。
//
// 用法：
//
//	c, _ := NewFeastClient("feast-online:6566", "risk", 50*time.Millisecond)
//	defer c.Close()
//	feats, err := c.GetOnlineFeatures(ctx, "cust123",
//	    []string{"customer:paid_count_90d", "customer:chargeback_count_90d"})
type FeastClient struct {
	conn    *grpc.ClientConn
	client  servingClient
	project string
	timeout time.Duration
	// FeatureService Feast feature service 名（跟 DS 约定）。
	// 见 docs/FEAST_INTEGRATION.md
	FeatureService string
}

// NewFeastClient dial Feast online serving。timeout 0 默认 50ms。
//
// 注意：当前实现 client = nil（proto 未 vendor）。ML 团队接入时在这里
// 把 grpc conn 包成 serving.NewServingServiceClient(conn)。
func NewFeastClient(addr, project string, timeout time.Duration) (*FeastClient, error) {
	if addr == "" {
		return nil, fmt.Errorf("feast: empty addr")
	}
	if timeout <= 0 {
		timeout = 50 * time.Millisecond
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("feast: dial %s: %w", addr, err)
	}
	return &FeastClient{
		conn:           conn,
		client:         nil, // TODO(ml-team): wire serving.NewServingServiceClient(conn)
		project:        project,
		timeout:        timeout,
		FeatureService: "risk_realtime_v1",
	}, nil
}

// GetOnlineFeatures 取一个 entity 的一组 feature。
//
// featureRefs 形如 ["customer:paid_count_90d", "customer:chargeback_count_90d"]。
// 返回 map[short_name]float64（剥掉 "customer:" 前缀方便 caller）。
//
// fail-open 语义：超时 / RPC error / client 未连 都返 (empty_map, err)，
// 调用方应当用 0 兜底（log warn）。
func (c *FeastClient) GetOnlineFeatures(ctx context.Context, customerID string, featureRefs []string) (map[string]float64, error) {
	if c == nil || c.client == nil {
		return nil, ErrFeastNotEnabled
	}
	if customerID == "" || len(featureRefs) == 0 {
		return map[string]float64{}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req := &onlineRequest{
		FeatureService: c.FeatureService,
		Project:        c.project,
		EntityRows:     []map[string]string{{"customer_id": customerID}},
		FeatureRefs:    featureRefs,
	}
	resp, err := c.client.GetOnlineFeatures(ctx, req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, ErrFeastTimeout
		}
		return nil, fmt.Errorf("feast get: %w", err)
	}
	if len(resp.Values) == 0 {
		return map[string]float64{}, nil
	}

	out := make(map[string]float64, len(resp.FeatureNames))
	for i, name := range resp.FeatureNames {
		if i >= len(resp.Values[0]) {
			log.Printf("feast: warn: feature %s missing in response, defaulting 0", name)
			continue
		}
		// 剥 "view:" 前缀
		short := name
		for j := 0; j < len(name); j++ {
			if name[j] == ':' {
				short = name[j+1:]
				break
			}
		}
		out[short] = resp.Values[0][i]
	}
	return out, nil
}

// Close 释放 gRPC 连接。
func (c *FeastClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
