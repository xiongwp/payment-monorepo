// httprpc.go: rpcClient interface 的 HTTP+SSE 实现。
//
// 跟服务端 internal/server/http_api.go 配对：
//   - GET   /api/v1/configs/:ns/:key       一次性 Get
//   - GET   /api/v1/configs/:ns/watch?...  SSE long-stream
//
// 设计：
//   - 单 baseURL；多 endpoint 时 caller 自己包 etcd resolver 包一层 round_robin
//     （现阶段 SDK 用一个 endpoint 已经够；多副本通过 server 端 Kafka bridge fan-out）
//   - mTLS：caller 提供 *http.Client（tls.Config 已配好）；不强求
//   - SSE 客户端不重连：watchLoop 已经在 client.go 里负责退避重连，
//     这里 Watch 一次断了就 return error 让 caller 自决
//
// 用法：
//
//	httpcli := &http.Client{Timeout: 0, Transport: tlsTransport}
//	rpc := configcenter.NewHTTPClient("https://config-center:9690", httpcli)
//	cli, _ := configcenter.NewWithRPC(rpc, configcenter.Config{
//	    Namespace: "card-payment",
//	    InstanceID: "pod-abc",
//	})
package configcenter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTPClient 实现 rpcClient。
type HTTPClient struct {
	baseURL string
	httpc   *http.Client
}

// NewHTTPClient 构造。endpoint 形如 "https://config-center.payment.local:9690"。
// httpc 为 nil → 用 http.DefaultClient（dev only；生产必须给 mTLS Transport）。
//
// **超时纪律**：
//   - Get：调用方 ctx 超时控制；不在这层加。
//   - Watch：长连接不能 Timeout（http.Client.Timeout 会强切 SSE）；caller
//     必须传 timeout=0 的 client，或用本函数内部的 cloned client（见下）。
//
// 本函数自动 clone client 把 Timeout 清 0，保证 SSE 不被砍。
func NewHTTPClient(endpoint string, httpc *http.Client) *HTTPClient {
	endpoint = strings.TrimRight(endpoint, "/")
	if httpc == nil {
		httpc = http.DefaultClient
	}
	// SSE 必须 Timeout=0；clone 一份避免污染 caller 的 client
	streamClient := *httpc
	streamClient.Timeout = 0
	return &HTTPClient{
		baseURL: endpoint,
		httpc:   &streamClient,
	}
}

// Get 单 key 取。
//
// 404 → 返 nil, ErrNotFound（让 SDK 区分"key 不存在" vs"strategy 没命中"）
// 200 → 解 ConfigWire → ConfigValue
func (c *HTTPClient) Get(ctx context.Context, ns, key, instanceID string) (*ConfigValue, error) {
	u := fmt.Sprintf("%s/api/v1/configs/%s/%s?instance_id=%s",
		c.baseURL,
		url.PathEscape(ns),
		url.PathEscape(key),
		url.QueryEscape(instanceID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var w wireConfig
		if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
			return nil, fmt.Errorf("decode get response: %w", err)
		}
		return wireToConfigValue(&w), nil
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get config %s/%s: http %d: %s", ns, key, resp.StatusCode, body)
	}
}

// Watch 启动 SSE 长连；onEvent 每来一行 event 调一次。
//
// 阻塞直到：
//   - ctx 取消（caller 主动停 / SDK 重连 backoff 触发）
//   - server 关连接 / 网络错误（client.go 的 watchLoop 会退避重连）
//
// SSE 协议解析：
//   - 行 "event: <type>" 记下 type
//   - 行 "id: <ver>"      可选；记 last-event-id（这里不显式用，SDK 自己跟 maxVersion）
//   - 行 "data: <json>"   解 ConfigWire；空行 → flush 一个 event
//   - 以 ":" 开头的行（comment）→ 忽略（heartbeat）
func (c *HTTPClient) Watch(ctx context.Context, ns, instanceID string, sinceVersion int64,
	onEvent func(ev *WatchEvent)) error {

	u := fmt.Sprintf("%s/api/v1/configs/%s/watch?instance_id=%s&since_version=%d",
		c.baseURL,
		url.PathEscape(ns),
		url.QueryEscape(instanceID),
		sinceVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("watch %s: http %d: %s", ns, resp.StatusCode, body)
	}
	return parseSSE(resp.Body, onEvent)
}

// parseSSE 标准 SSE 行解析；按空行 flush event。
//
// 单 event 累积：
//
//	event: update
//	id: 42
//	data: {"namespace":"x", ...}
//	<空行>
func parseSSE(r io.Reader, onEvent func(ev *WatchEvent)) error {
	br := bufio.NewReaderSize(r, 1<<16)
	var (
		eventType string
		dataBuf   bytes.Buffer
	)
	flush := func() {
		defer func() {
			eventType = ""
			dataBuf.Reset()
		}()
		if dataBuf.Len() == 0 {
			return
		}
		var w wireConfig
		if err := json.Unmarshal(dataBuf.Bytes(), &w); err != nil {
			// 单条坏不致命；下一条继续
			return
		}
		ev := &WatchEvent{
			Type:   eventTypeFromName(eventType),
			Config: wireToConfigValue(&w),
		}
		onEvent(ev)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				flush()
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			// comment / heartbeat
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(line[len("event:"):])
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimSpace(line[len("data:"):]))
			continue
		}
		// id: / retry: 暂不显式处理
	}
}

// ─── wire types ──────────────────────────────────────────────────────────

type wireConfig struct {
	Namespace    string     `json:"namespace"`
	Key          string     `json:"key"`
	Version      int64      `json:"version"`
	Value        string     `json:"value"`
	Format       string     `json:"format"`
	EffectiveAt  *time.Time `json:"effective_at,omitempty"`
	ExpireAt     *time.Time `json:"expire_at,omitempty"`
	Strategy     string     `json:"strategy"`
	StrategySpec string     `json:"strategy_spec,omitempty"`
	UpdatedBy    string     `json:"updated_by"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func wireToConfigValue(w *wireConfig) *ConfigValue {
	if w == nil {
		return nil
	}
	cv := &ConfigValue{
		Namespace: w.Namespace,
		Key:       w.Key,
		Value:     w.Value,
		Format:    w.Format,
		Version:   w.Version,
		UpdatedBy: w.UpdatedBy,
		UpdatedAt: w.UpdatedAt,
	}
	if w.EffectiveAt != nil {
		cv.EffectiveAt = *w.EffectiveAt
	}
	if w.ExpireAt != nil {
		cv.ExpireAt = *w.ExpireAt
	}
	return cv
}

func eventTypeFromName(s string) EventType {
	switch s {
	case "snapshot":
		return EventSnapshot
	case "update":
		return EventUpdate
	case "delete":
		return EventDelete
	}
	return EventUnknown
}

// 让 strconv import 不被裁掉（id: line future use）
var _ = strconv.ParseInt
