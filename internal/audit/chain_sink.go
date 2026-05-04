// chain_sink.go: 链式签名 audit sink。
//
// 合规取证场景：监管 / 法务要"这条 audit 没被改过 / 顺序没被重排"。技术保证：
//
//	row_hash[N] = sha256(prev_hash[N] || canonical_json(record[N]))
//	prev_hash[N] = row_hash[N-1]
//	prev_hash[0] = "0..0"  (genesis)
//
// 任何一行 record 被后期篡改 / 删除 / 重排，后续所有 row_hash 都对不上 →
// 一行验证失败立刻知道。
//
// canonical_json：JSON 序列化用 sort-keys + 紧凑（无空格）保证不同写入端
// 计算得到的 hash 一致。
//
// 性能：链式签名引入串行（每条 record 必须等前一条算完 row_hash）。轻量
// 计算（< 10µs sha256）；高 QPS 场景用 batch + 异步刷盘抵消。
//
// 验证：cmd/audit-verify CLI（本仓暂未提供）从头开始扫所有 record，对每条
// 重算 row_hash，跟落库的对比。
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
)

// genesisHash 初始 prev_hash。所有 zeros 让 verifier 知道 "这是链头"。
const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ChainSink wraps an inner Sink, prepending hash chain fields to every record.
// 实现 audit.Sink 接口；可塞到 MultiSink 里跟 LogSink / MemSink / Encrypt 并存。
//
// inner 拿到的 record 已经被填充了 PrevHash / RowHash 字段（写在 metadata 上，
// 避免改 DecisionAudit struct 字段引起 schema 漂移）。
type ChainSink struct {
	Inner Sink

	mu       sync.Mutex
	prevHash string
}

// NewChainSink 起点 prev=genesis。生产从 DB 拿"已落库最后一行的 row_hash"
// 作为 prev，可通过 NewChainSinkResume 注入。
func NewChainSink(inner Sink) *ChainSink {
	return &ChainSink{Inner: inner, prevHash: genesisHash}
}

// NewChainSinkResume 用历史 prev_hash 续写（服务重启时从持久化 sink 拉最后一条）。
func NewChainSinkResume(inner Sink, lastRowHash string) *ChainSink {
	if lastRowHash == "" {
		lastRowHash = genesisHash
	}
	return &ChainSink{Inner: inner, prevHash: lastRowHash}
}

// Write 计算 row_hash + 写到 record.Input.Metadata（chain_prev_hash / chain_row_hash），
// 然后转发给 inner sink。失败不重试（fail-open，不阻塞 Screen 主路径），但会
// 注意：如果 inner 写失败，我们仍然推进 prev_hash → 链不会"卡住"，但物理
// 持久化里会有 gap。verifier 看到 gap 即认定"链断"。生产可改成失败时
// 回滚 prev_hash + log 严重告警。
func (s *ChainSink) Write(ctx context.Context, a *DecisionAudit) {
	if a == nil || s.Inner == nil {
		return
	}
	s.mu.Lock()
	prev := s.prevHash
	row := computeRowHash(prev, a)
	s.prevHash = row
	s.mu.Unlock()

	if a.Input.Metadata == nil {
		a.Input.Metadata = make(map[string]string, 2)
	}
	a.Input.Metadata["chain_prev_hash"] = prev
	a.Input.Metadata["chain_row_hash"] = row
	s.Inner.Write(ctx, a)
}

// computeRowHash sha256(prev_hash || canonical_json(audit-record-without-chain-fields))。
// 把 chain_* metadata 排除是为了避免循环依赖（计算 hash 时 record 不能含 row_hash）。
func computeRowHash(prev string, a *DecisionAudit) string {
	body := canonicalize(a)
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalize 把 DecisionAudit 序列化为稳定 JSON：
//   - 去掉 metadata 里的 chain_* 字段（避免循环）
//   - sorted keys
//   - 紧凑无空格
//
// JSON 序列化默认 map 顺序不稳；我们手动重排 metadata，剩下用 encoder
// 自带顺序（struct fields 是定义顺序，已稳定）。
func canonicalize(a *DecisionAudit) []byte {
	cp := *a
	if cp.Input.Metadata != nil {
		filtered := make(map[string]string, len(cp.Input.Metadata))
		for k, v := range cp.Input.Metadata {
			if k == "chain_prev_hash" || k == "chain_row_hash" {
				continue
			}
			filtered[k] = v
		}
		cp.Input.Metadata = sortedMap(filtered)
	}
	body, _ := json.Marshal(&cp)
	return body
}

// sortedMap 把 map 转成 sort-keys 形式（Go map JSON marshal 已经按 key 字典序，
// 实测 encoding/json 保证 map[string]string 有序输出；这里多包一层是为了
// 显式表达"我们依赖有序"，未来 encoding/json 行为变了也好定位）。
func sortedMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(m))
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}

// VerifyChain 验证一段 audit record（按 occurred_at 升序传入）整条链的完整性。
// 起点 prev = startPrev（""→genesis）。返回首个对不上的 index 和具体 hash 差异；
// 全部正确返回 (-1, nil)。
//
// 监管对账 / cron job 用。
func VerifyChain(records []*DecisionAudit, startPrev string) (int, error) {
	if startPrev == "" {
		startPrev = genesisHash
	}
	prev := startPrev
	for i, r := range records {
		gotPrev := r.Input.Metadata["chain_prev_hash"]
		gotRow := r.Input.Metadata["chain_row_hash"]
		if gotPrev != prev {
			return i, &VerifyError{Index: i, Field: "prev_hash", Want: prev, Got: gotPrev}
		}
		want := computeRowHash(prev, r)
		if gotRow != want {
			return i, &VerifyError{Index: i, Field: "row_hash", Want: want, Got: gotRow}
		}
		prev = gotRow
	}
	return -1, nil
}

// VerifyError chain 验证失败的具体偏差。
type VerifyError struct {
	Index int
	Field string // prev_hash | row_hash
	Want  string
	Got   string
}

func (e *VerifyError) Error() string {
	return "chain verify failed at index " + itoa(e.Index) + " field=" + e.Field +
		" want=" + e.Want + " got=" + e.Got
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	if i < 0 {
		return "-" + itoa(-i)
	}
	const digits = "0123456789"
	if i < 10 {
		return string(digits[i])
	}
	return itoa(i/10) + string(digits[i%10])
}
