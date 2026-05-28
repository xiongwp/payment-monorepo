// chain_stream.go: 通用的 tamper-evident chain，用于 DecisionAudit 之外的
// 高敏 stream（review state changes / rule audit / outcome labels）。
//
// 核心算法跟 chain_sink.go 完全一致（sha256(prev_hash || canonical_json(record))），
// 但 record 类型是 `map[string]any` —— 各 stream 自己定义 schema、把 typed
// 结构序列化成 map 后塞进来。chain_sink.go 不动；这里只是把它"泛化"到任意
// JSON-able record。
//
// 设计：**每个 stream 一条独立链**（4 条链：decisions / review / rule_audit /
// outcomes），不是一条大链。理由：
//
//   - 各 stream 写入速率差异大（decisions ~1k/s, review ~10/s, rule_audit ~0.01/s）
//     混在一起需要全局锁，吞吐量瓶颈
//   - verify 时希望 per-stream 报告（"review 链断在 idx=42"），合并链定位困难
//   - 不同 stream 落库后端可能不同（CH / PG / S3），合并链需要全部齐才能 verify
//
// 失败兜底：inner Write 返 err / panic 时仍然推进 prev_hash（chain 不会"卡住"），
// 但记 metric (risk_audit_chain_write_total{stream=X,result=err})；监控告警 +
// verify 时表现为 row gap → 视为篡改的同样信号。
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
)

// StreamRecord 已签名的 chain record。Stream 是 "review" / "rule_audit" /
// "outcomes" 等流名；Body 是序列化后的 record 内容（schema 由 caller 定义）；
// PrevHash / RowHash 是链字段。
//
// 落库形态：可直接 Marshal 成一行 JSON 进 file/kafka/CH。verify 时按 OccurredAt
// 升序拿到 []*StreamRecord，重算 row_hash 对比 RowHash 字段。
type StreamRecord struct {
	Stream   string         `json:"stream"`
	Body     map[string]any `json:"body"`
	PrevHash string         `json:"chain_prev_hash"`
	RowHash  string         `json:"chain_row_hash"`
}

// StreamSink 任意 stream 的存储后端。实现需无阻塞；持久化层（CH/PG/file）应
// 内部用 channel buffer + worker。返回 error 仅用于本地计数（chain 仍推进，
// 见包注释）。
type StreamSink interface {
	WriteRecord(ctx context.Context, r *StreamRecord) error
}

// MemStreamSink 进程内 ring buffer。给 /admin/audit/chain/verify?stream=X
// 端点用；生产应换 ClickHouse 长留存。
type MemStreamSink struct {
	mu     sync.RWMutex
	cap    int
	buf    []*StreamRecord
	cursor int
	full   bool
}

func NewMemStreamSink(cap int) *MemStreamSink {
	if cap <= 0 {
		cap = 4096
	}
	return &MemStreamSink{cap: cap, buf: make([]*StreamRecord, cap)}
}

func (s *MemStreamSink) WriteRecord(_ context.Context, r *StreamRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf[s.cursor] = r
	s.cursor = (s.cursor + 1) % s.cap
	if s.cursor == 0 {
		s.full = true
	}
	return nil
}

// Recent 返回最近 limit 条（newest-first，跟 MemSink.Recent 一致）。
func (s *MemStreamSink) Recent(limit int) []*StreamRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	size := s.cursor
	if s.full {
		size = s.cap
	}
	if limit <= 0 || limit > size {
		limit = size
	}
	out := make([]*StreamRecord, 0, limit)
	for i := 0; i < limit; i++ {
		idx := (s.cursor - 1 - i + s.cap) % s.cap
		if s.buf[idx] != nil {
			out = append(out, s.buf[idx])
		}
	}
	return out
}

// ChainWriter 通用 chain writer。**每个 stream 一个独立实例**（独立 prev_hash
// 状态），保证 4 条链互不影响。
//
// 用法：
//
//	cw := audit.NewChainWriter("review", memSink)
//	cw.Append(ctx, map[string]any{"op":"claim", "case_id":"abc", "actor":"alice"})
type ChainWriter struct {
	stream string
	inner  StreamSink

	mu       sync.Mutex
	prevHash string
}

// NewChainWriter 起点 prev=genesis（chain_sink.go 里的常量）。生产从持久 sink
// 拉最后一条 row_hash 续写：NewChainWriterResume。
func NewChainWriter(stream string, inner StreamSink) *ChainWriter {
	return &ChainWriter{stream: stream, inner: inner, prevHash: genesisHash}
}

// NewChainWriterResume 同 NewChainWriter，但 prev=lastRowHash（服务重启续链）。
func NewChainWriterResume(stream string, inner StreamSink, lastRowHash string) *ChainWriter {
	if lastRowHash == "" {
		lastRowHash = genesisHash
	}
	return &ChainWriter{stream: stream, inner: inner, prevHash: lastRowHash}
}

// Append 计算 row_hash 并把 record 转发给 inner。
//
// 失败行为：inner 写失败 chain **仍推进 prev_hash**（链不卡住，便于后续 verify
// 看到 gap 即视为篡改）。返回 inner 的 err，调用方一般 ignore（用 metric 监控）。
//
// nil-safe：cw == nil → no-op（cfg 关闭时所有 wrapper 走 noop 路径）。
func (cw *ChainWriter) Append(ctx context.Context, body map[string]any) error {
	if cw == nil || cw.inner == nil {
		return nil
	}
	cw.mu.Lock()
	prev := cw.prevHash
	row := computeStreamRowHash(prev, cw.stream, body)
	cw.prevHash = row
	cw.mu.Unlock()

	rec := &StreamRecord{
		Stream:   cw.stream,
		Body:     body,
		PrevHash: prev,
		RowHash:  row,
	}
	return cw.inner.WriteRecord(ctx, rec)
}

// Stream 返回链名（review / rule_audit / outcomes / ...）。
func (cw *ChainWriter) Stream() string {
	if cw == nil {
		return ""
	}
	return cw.stream
}

// computeStreamRowHash sha256(prev || canonical_json({stream, body}))。
// stream 也参与 hash，防止 cross-stream record swap 攻击（把一条 outcomes
// record 改 stream 字段冒充 review 记录）。
func computeStreamRowHash(prev, stream string, body map[string]any) string {
	payload := struct {
		Stream string         `json:"stream"`
		Body   map[string]any `json:"body"`
	}{
		Stream: stream,
		Body:   sortedAnyMap(body),
	}
	b, _ := json.Marshal(payload)
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// sortedAnyMap encoding/json 对 map[string]any 默认按 key 字典序输出，但显式
// 重排有助于"未来 encoding/json 行为变更也能保持稳定 hash"。嵌套 map 不递归
// 排序（caller schema 已扁平，参考 review/feedback chain.go 注释）。
func sortedAnyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return m
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(m))
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}

// VerifyStreamChain 验证某一流的链完整性，跟 VerifyChain（DecisionAudit 版）
// 接口对应。records 必须按 OccurredAt 升序传入。startPrev 空 → genesis。
// 返回首个对不上的 index；全部正确返 (-1, nil)。
func VerifyStreamChain(records []*StreamRecord, startPrev string) (int, error) {
	if startPrev == "" {
		startPrev = genesisHash
	}
	prev := startPrev
	for i, r := range records {
		if r == nil {
			return i, &VerifyError{Index: i, Field: "record_nil"}
		}
		if r.PrevHash != prev {
			return i, &VerifyError{Index: i, Field: "prev_hash", Want: prev, Got: r.PrevHash}
		}
		want := computeStreamRowHash(prev, r.Stream, r.Body)
		if r.RowHash != want {
			return i, &VerifyError{Index: i, Field: "row_hash", Want: want, Got: r.RowHash}
		}
		prev = r.RowHash
	}
	return -1, nil
}
