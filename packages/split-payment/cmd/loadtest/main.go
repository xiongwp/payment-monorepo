// Package main — 轮换账户 E2E 压测客户端
//
// 通过 split-payment 的 TriggerEvent gRPC 驱动 4 大资金流（topup/payment/
// transfer/withdraw），度量端到端 TPS / latency / 错误率，并按资金流类型分桶。
//
// 部署：使用 packages/split-payment/deploy/loadtest/Dockerfile.loadtest 构建。
//
// 流量模式（config.load.mode）：
//   - mixed:        按 mix_ratio 加权随机挑 4 种资金流
//   - topup/payment/transfer/withdraw: 单一资金流
//   - router-only:  直接打 accounting.RouterDryRun（不走 split-payment, 测纯路由 TPS）
//
// 输出：
//   - 控制台：每 N 秒一次进度行
//   - 文件：JSON 报告，含 p50/p90/p95/p99 + 每资金流细分 + 错误分布
//
// 注意：本压测假定 graph 已经预注册（scripts/bootstrap-logical-accounts.sh 做）。
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	mathrand "math/rand"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cloudwego/kitex/client"

	spadmin "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1"
	spadminsvc "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1/adminservice"
	"github.com/spf13/viper"
)

var buildVersion = "dev"

// ─── Account Pool ─────────────────────────────────────────────────────────
//
// loadtest 启动期从 /output/account_pool.json 读进内存（由 loadtest-bootstrap
// 在压测前预创建账户后写出）。dispatch() 4 种资金流的 event.attrs 全部从这个 pool 里
// 取真实 account_no（19 位数字串），而不是硬编码 "ch-12/recv" 这种 accounting 不认的字符串。
//
// 与 graphCache 同思路：启动期写一次，之后只读 → goroutine 安全。
// 文件不存在不算错（router-only 模式下可以跳过；mixed 模式发到 TriggerEvent 时
// accounting 会报 "account not found"，错信号清晰）。

type accountPool struct {
	Currency         string             `json:"currency"`
	Users            []string           `json:"users"`             // user balance accounts
	Merchants        []string           `json:"merchants"`         // merchant balance accounts
	MerchantPendings []string           `json:"merchant_pendings"` // merchant pending settle
	Channels         []channelAccounts  `json:"channels"`          // 渠道下 4 个子账户
	Platform         platformAccounts   `json:"platform"`          // 3 个全局平台账户
}

type channelAccounts struct {
	Receivable string `json:"recv"`
	Suspense   string `json:"suspense"`
	Fee        string `json:"fee"`
	Payable    string `json:"payable"`
}

type platformAccounts struct {
	FeeClearing     string `json:"fee_clearing"`
	FeeRevenue      string `json:"fee_revenue"`
	WithdrawPending string `json:"withdraw_pending"`
}

var pool *accountPool // 启动期填，runtime 只读

func loadAccountPool(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var p accountPool
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("parse pool %s: %w", path, err)
	}
	if len(p.Users) == 0 || len(p.Channels) == 0 || len(p.Merchants) == 0 {
		return fmt.Errorf("pool %s 缺数据: users=%d merchants=%d channels=%d",
			path, len(p.Users), len(p.Merchants), len(p.Channels))
	}
	pool = &p
	return nil
}

// 取 modulo 索引，方便 worker 用 random user_id 落到 pool 已有的账户上。
func (p *accountPool) user(i int64) string {
	return p.Users[int(i)%len(p.Users)]
}
func (p *accountPool) merchant(i int) string {
	return p.Merchants[(i-1)%len(p.Merchants)]
}
func (p *accountPool) merchantPending(i int) string {
	return p.MerchantPendings[(i-1)%len(p.MerchantPendings)]
}
func (p *accountPool) channel(i int64) channelAccounts {
	return p.Channels[(int(i)-1)%len(p.Channels)]
}

// graphCache: router-only 模式下不走 TriggerEvent（要 accounting 配齐账户），改走
// DryRun（split-payment 内部只翻译 + 校验，不落账）。但 DryRun 是 stateless 的 ——
// 每次 RPC 都要把整个 Graph spec 一起塞进去（不是按 key 查 server-side）。
// 所以启动期一次性把所有 graph json 从盘上读进内存，dispatch 直接从 map 里拿。
//
// 为什么读盘而非 spClient.GetGraph：
//   1. 不依赖 split-payment 已 seed 完成（避免启动 race）
//   2. 不依赖 split-payment 服务可用（启动期失败可立即定位是配置问题）
//   3. 同一份 json 文件 split-payment 的 seed 也是读它（MONEYFLOW_SEED_DIR），
//      天然保证 loadtest 看到的 graph spec 跟 split-payment 处理的一致
//
// 注意：graphCache 只在 main 启动期写一次，之后只读 → goroutine 安全。
var graphCache map[string]*spadmin.Graph

// loadGraphsFromDir 从 dir/*.json 读所有 graph，按 key 索引。
//
// graph json 的顶层结构（见 deploy/loadtest/graphs/topup.json）：
//   { "key": "user-topup-v1", "name": "...", "version": "...", "status": "active", "spec": { ... } }
//
// spadmin.Graph 的 SpecJson 是 spec 子对象的 JSON 字面量（不是整个文件）—— 同 GetGraph
// 返回的格式。这里读完整 json，把 spec 提出来重新 marshal 成 SpecJson。
func loadGraphsFromDir(dir string, keys map[string]string) error {
	graphCache = make(map[string]*spadmin.Graph, len(keys))

	// 把配置里的 key 集合反向成 set，方便文件 → key 匹配
	wantKeys := make(map[string]bool, len(keys))
	for _, gk := range keys {
		if gk != "" {
			wantKeys[gk] = true
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := dir + "/" + e.Name()
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		// 顶层 envelope：{ key, name, version, status, spec }
		var envelope struct {
			Key     string          `json:"key"`
			Name    string          `json:"name"`
			Version string          `json:"version"`
			Status  string          `json:"status"`
			Spec    json.RawMessage `json:"spec"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if envelope.Key == "" {
			fmt.Printf("  WARN: %s 没有 key 字段，跳过\n", path)
			continue
		}
		if !wantKeys[envelope.Key] {
			// 文件存在但 config 没引用，跳过（不算错）
			continue
		}

		graphCache[envelope.Key] = &spadmin.Graph{
			Key:      envelope.Key,
			Name:     envelope.Name,
			Version:  envelope.Version,
			Status:   envelope.Status,
			SpecJson: []byte(envelope.Spec), // RawMessage 直接转 []byte
		}
		fmt.Printf("  cached graph %s ← %s (spec=%d bytes)\n", envelope.Key, path, len(envelope.Spec))
	}

	// 校验配置里要的 key 都拿到了
	for _, gk := range keys {
		if gk != "" && graphCache[gk] == nil {
			return fmt.Errorf("graph %q referenced in config.flow.graph_keys but not found in %s", gk, dir)
		}
	}
	return nil
}

// ─── 配置 ─────────────────────────────────────────────────────────────────

type targetConfig struct {
	Mode                  string `mapstructure:"mode"`
	SplitPaymentEndpoint  string `mapstructure:"split_payment_endpoint"`
	AccountingEndpoint    string `mapstructure:"accounting_endpoint"`
	AccountingAdminHTTP   string `mapstructure:"accounting_admin_http"`
	AdminToken            string `mapstructure:"admin_token"`
}

type loadConfig struct {
	Concurrency  int           `mapstructure:"concurrency"`
	Duration     time.Duration `mapstructure:"duration"`
	RateLimitQPS int           `mapstructure:"rate_limit_qps"`
	Warmup       time.Duration `mapstructure:"warmup"`
	Mode         string        `mapstructure:"mode"`
	MixRatio     map[string]int `mapstructure:"mix_ratio"`
}

type flowConfig struct {
	ChannelCount     int                `mapstructure:"channel_count"`
	UserCount        int                `mapstructure:"user_count"`
	AmountMinorRange [2]int64           `mapstructure:"amount_minor_range"`
	Currency         string             `mapstructure:"currency"`
	GraphKeys        map[string]string  `mapstructure:"graph_keys"`
}

type chaosConfig struct {
	Enabled                bool          `mapstructure:"enabled"`
	ForceSwitchInterval    time.Duration `mapstructure:"force_switch_interval"`
	ForceProvisionInterval time.Duration `mapstructure:"force_provision_interval"`
}

type reportConfig struct {
	OutputPath            string  `mapstructure:"output_path"`
	LatencyBucketsMs      []int64 `mapstructure:"latency_buckets_ms"`
	ProgressInterval      time.Duration `mapstructure:"progress_interval"`
	PrometheusPushgateway string  `mapstructure:"prometheus_pushgateway"`
}

type config struct {
	Target targetConfig `mapstructure:"target"`
	Load   loadConfig   `mapstructure:"load"`
	Flow   flowConfig   `mapstructure:"flow"`
	Chaos  chaosConfig  `mapstructure:"chaos"`
	Report reportConfig `mapstructure:"report"`
}

// ─── 资金流类型 + 加权选择 ─────────────────────────────────────────────────

type flowKind string

const (
	flowTopup    flowKind = "topup"
	flowPayment  flowKind = "payment"
	flowTransfer flowKind = "transfer"
	flowWithdraw flowKind = "withdraw"
)

func allFlows() []flowKind {
	return []flowKind{flowTopup, flowPayment, flowTransfer, flowWithdraw}
}

// weightedPicker 根据 mix_ratio 加权挑资金流。
type weightedPicker struct {
	flows  []flowKind
	cumSum []int
	total  int
}

func newWeightedPicker(mix map[string]int) *weightedPicker {
	p := &weightedPicker{}
	for _, f := range allFlows() {
		w := mix[string(f)]
		if w <= 0 {
			continue
		}
		p.flows = append(p.flows, f)
		p.total += w
		p.cumSum = append(p.cumSum, p.total)
	}
	if p.total == 0 {
		// 均匀
		for _, f := range allFlows() {
			p.flows = append(p.flows, f)
			p.total++
			p.cumSum = append(p.cumSum, p.total)
		}
	}
	return p
}

func (p *weightedPicker) pick(r *mathrand.Rand) flowKind {
	v := r.Intn(p.total)
	for i, c := range p.cumSum {
		if v < c {
			return p.flows[i]
		}
	}
	return p.flows[len(p.flows)-1]
}

// ─── 统计 ─────────────────────────────────────────────────────────────────

// stats 单一资金流的统计。所有原子操作。
type stats struct {
	ops        atomic.Int64
	errs       atomic.Int64
	totalLatNs atomic.Int64

	// 直方图：固定 bucket（毫秒边界），每 bucket 一个原子计数。
	bucketBounds []int64 // ms
	bucketHits   []atomic.Int64

	// 错误样本（前 5 种 distinct msg），帮助调试。
	// 简化：sync.Map 存 errMsg → count；输出时取前 5。
	errSamples sync.Map
}

func newStats(bucketsMs []int64) *stats {
	s := &stats{
		bucketBounds: append([]int64(nil), bucketsMs...),
	}
	s.bucketHits = make([]atomic.Int64, len(bucketsMs)+1) // +1 for overflow bucket
	return s
}

func (s *stats) record(latency time.Duration, err error) {
	s.ops.Add(1)
	if err != nil {
		s.errs.Add(1)
		// 取前 60 字符做 key，避免 unique flow_id 撑爆 sync.Map
		key := err.Error()
		if len(key) > 60 {
			key = key[:60]
		}
		if v, ok := s.errSamples.Load(key); ok {
			cnt := v.(*atomic.Int64)
			cnt.Add(1)
		} else {
			cnt := &atomic.Int64{}
			cnt.Store(1)
			s.errSamples.Store(key, cnt)
		}
	}
	ns := latency.Nanoseconds()
	s.totalLatNs.Add(ns)

	ms := ns / int64(time.Millisecond)
	// 找第一个 >= ms 的 bucket
	idx := sort.Search(len(s.bucketBounds), func(i int) bool { return s.bucketBounds[i] >= ms })
	s.bucketHits[idx].Add(1)
}

// topErrors 返回 cnt 最多的 N 个错误样本。
func (s *stats) topErrors(n int) []struct {
	Msg   string
	Count int64
} {
	type pair struct {
		Msg   string
		Count int64
	}
	var all []pair
	s.errSamples.Range(func(k, v any) bool {
		all = append(all, pair{k.(string), v.(*atomic.Int64).Load()})
		return true
	})
	sort.Slice(all, func(i, j int) bool { return all[i].Count > all[j].Count })
	if len(all) > n {
		all = all[:n]
	}
	out := make([]struct {
		Msg   string
		Count int64
	}, len(all))
	for i, p := range all {
		out[i].Msg = p.Msg
		out[i].Count = p.Count
	}
	return out
}

// percentile 用直方图线性插值估 p（0-100）
func (s *stats) percentile(p float64) int64 {
	total := int64(0)
	for i := range s.bucketHits {
		total += s.bucketHits[i].Load()
	}
	if total == 0 {
		return 0
	}
	target := int64(float64(total) * p / 100.0)
	acc := int64(0)
	for i := range s.bucketHits {
		acc += s.bucketHits[i].Load()
		if acc >= target {
			if i < len(s.bucketBounds) {
				return s.bucketBounds[i]
			}
			// overflow bucket → 用最后一个上限的 1.5x 当估值
			return s.bucketBounds[len(s.bucketBounds)-1] * 2
		}
	}
	return 0
}

func (s *stats) avgLatencyMs() float64 {
	ops := s.ops.Load()
	if ops == 0 {
		return 0
	}
	return float64(s.totalLatNs.Load()) / float64(ops) / float64(time.Millisecond)
}

// ─── Worker ───────────────────────────────────────────────────────────────

type worker struct {
	id        int
	cfg       *config
	picker    *weightedPicker
	spClient  spadminsvc.Client
	rng       *mathrand.Rand
	flowStats map[flowKind]*stats // 共享指针，所有 worker 同一组
	mainStats *stats              // 总汇
	limiter   <-chan struct{}     // 全局限速通道；nil = 不限速
}

func (w *worker) run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if w.limiter != nil {
			select {
			case <-ctx.Done():
				return
			case <-w.limiter:
			}
		}

		flow := w.pickFlow()
		start := time.Now()
		err := w.dispatch(ctx, flow)
		elapsed := time.Since(start)

		w.mainStats.record(elapsed, err)
		if fs := w.flowStats[flow]; fs != nil {
			fs.record(elapsed, err)
		}
	}
}

func (w *worker) pickFlow() flowKind {
	switch w.cfg.Load.Mode {
	case "mixed":
		return w.picker.pick(w.rng)
	case "router-only":
		// router-only 模式下 dispatch 走 DryRun（不打 accounting），但 graph 翻译
		// 还要正常的 event payload —— 所以仍按 mix_ratio 选一个真实资金流，event
		// 字段全填，只是最后调用 DryRun 而不是 TriggerEvent。
		return w.picker.pick(w.rng)
	case string(flowTopup), string(flowPayment), string(flowTransfer), string(flowWithdraw):
		return flowKind(w.cfg.Load.Mode)
	default:
		return flowTopup
	}
}

// dispatch 真正发请求。
// 通过 split-payment 的 TriggerEvent gRPC，graph_key 选择资金流，event_json 描述业务参数。
func (w *worker) dispatch(ctx context.Context, flow flowKind) error {
	// 生成业务参数
	userID := int64(w.rng.Intn(maxOr(w.cfg.Flow.UserCount, 1)))
	channelID := int64(w.rng.Intn(maxOr(w.cfg.Flow.ChannelCount, 1)) + 1)
	amount := w.cfg.Flow.AmountMinorRange[0] +
		w.rng.Int63n(maxOr64(w.cfg.Flow.AmountMinorRange[1]-w.cfg.Flow.AmountMinorRange[0], 1))
	flowID := newBizID(string(flow))

	// 资金流的 graph_key：从 config 读
	graphKey := w.cfg.Flow.GraphKeys[string(flow)]
	if graphKey == "" {
		// 默认按命名约定回退
		graphKey = "user-" + string(flow) + "-v1"
	}

	// event_json 是业务事件载荷；graph 期望 <node_id>_account / _amount / _currency
	// 三元组字段（见 seed/scenarios/user_topup.graph.json 的 _sample_trigger_payload）。
	//
	// 为简化端到端压测，每种 flow 的 graph 都是固定结构 → 这里硬编码字段集。
	// 业务侧不严谨（账户号是 fake 的）但 rotation 路径会完整走一遍：
	//   booking router → flow_anchor_route → tx_account_anchor → DoubleEntryBooking
	cur := w.cfg.Flow.Currency
	amtStr := fmt.Sprintf("%d", amount)
	peerUserID := int64(w.rng.Intn(maxOr(w.cfg.Flow.UserCount, 1)))

	// split-payment workflow.BusinessEvent / TriggerContext 期望的 JSON 结构：
	//   {
	//     "event":        "<event_code>",
	//     "amount_minor": <int64>,
	//     "currency":     "<cur>",
	//     "attributes":   { "<node_account_id_attr>": "<acct_id>",
	//                        "<node_account_id_attr>_amount": "<int>",
	//                        "<node_account_id_attr>_currency": "<cur>", ... }
	//   }
	// translator.resolveAccountID 读 tc.Attributes[node.AccountIDAttr] —— 必须放
	// 在 attributes 子对象里，不是顶层。之前放顶层导致 translator 报
	//   "edge X→Y from: attributes[\"X_account\"] missing"
	attrs := map[string]string{}

	// 每个 graph 的字段集 —— 全部塞进 attrs 子对象。
	//
	// account_id 来源：
	//   - pool != nil（loadtest-bootstrap 已跑过 + pool 文件已挂入）→ 真实 account_no
	//     （19 位数字字符串，accounting 真识别，TriggerEvent 能完整落 booking）
	//   - pool == nil → 退化到老的 "ch-N/recv" 这种 fake 字符串，仅 router-only
	//     模式可用（DryRun 不查 accounting）
	usePool := pool != nil
	userAcct := fmt.Sprintf("%d", userID)
	peerUserAcct := fmt.Sprintf("%d", peerUserID)
	if usePool {
		userAcct = pool.user(userID)
		peerUserAcct = pool.user(peerUserID)
	}

	switch flow {
	case flowTopup:
		// graph topup.json: channel_receivable → suspense → user_wallet, fee_clearing → channel_payable, platform_revenue
		net := amount - 100 // 留 100 分手续费
		if net < 1 {
			net = 1
		}
		chRecv := fmt.Sprintf("ch-%d/recv", channelID)
		chSus := fmt.Sprintf("ch-%d/suspense", channelID)
		chFee := fmt.Sprintf("ch-%d/fee", channelID)
		feeClearing := "platform/fee_clearing"
		feeRevenue := "platform/fee_revenue"
		if usePool {
			ch := pool.channel(channelID)
			chRecv, chSus, chFee = ch.Receivable, ch.Suspense, ch.Fee
			feeClearing = pool.Platform.FeeClearing
			feeRevenue = pool.Platform.FeeRevenue
		}
		attrs["channel_receivable_account"], attrs["channel_receivable_account_amount"], attrs["channel_receivable_account_currency"] = chRecv, amtStr, cur
		attrs["channel_suspense_account"], attrs["channel_suspense_account_amount"], attrs["channel_suspense_account_currency"] = chSus, amtStr, cur
		attrs["user_id_account"], attrs["user_id_account_amount"], attrs["user_id_account_currency"] = userAcct, fmt.Sprintf("%d", net), cur
		attrs["fee_clearing_account"], attrs["fee_clearing_account_amount"], attrs["fee_clearing_account_currency"] = feeClearing, "100", cur
		attrs["channel_fee_account"], attrs["channel_fee_account_amount"], attrs["channel_fee_account_currency"] = chFee, "60", cur
		attrs["fee_account"], attrs["fee_account_amount"], attrs["fee_account_currency"] = feeRevenue, "40", cur

	case flowPayment:
		// graph payment.json: user_wallet → merchant_pending → merchant_wallet
		merchantID := w.rng.Intn(100) + 1
		mPending := fmt.Sprintf("m-%d/pending", merchantID)
		mMain := fmt.Sprintf("m-%d", merchantID)
		if usePool {
			mPending = pool.merchantPending(merchantID)
			mMain = pool.merchant(merchantID)
		}
		attrs["payer_account"], attrs["payer_account_amount"], attrs["payer_account_currency"] = userAcct, amtStr, cur
		attrs["merchant_pending_account"], attrs["merchant_pending_account_amount"], attrs["merchant_pending_account_currency"] = mPending, amtStr, cur
		attrs["merchant_account"], attrs["merchant_account_amount"], attrs["merchant_account_currency"] = mMain, amtStr, cur

	case flowTransfer:
		// graph transfer.json: from_wallet → to_wallet
		attrs["from_account"], attrs["from_account_amount"], attrs["from_account_currency"] = userAcct, amtStr, cur
		attrs["to_account"], attrs["to_account_amount"], attrs["to_account_currency"] = peerUserAcct, amtStr, cur

	case flowWithdraw:
		// graph withdraw.json: user_wallet → withdraw_pending → channel_payable
		withdrawPending := "platform/withdraw_pending"
		chPayable := fmt.Sprintf("ch-%d/payable", channelID)
		if usePool {
			withdrawPending = pool.Platform.WithdrawPending
			chPayable = pool.channel(channelID).Payable
		}
		attrs["user_account"], attrs["user_account_amount"], attrs["user_account_currency"] = userAcct, amtStr, cur
		attrs["withdraw_pending_account"], attrs["withdraw_pending_account_amount"], attrs["withdraw_pending_account_currency"] = withdrawPending, amtStr, cur
		attrs["channel_payable_account"], attrs["channel_payable_account_amount"], attrs["channel_payable_account_currency"] = chPayable, amtStr, cur
	}

	event := map[string]any{
		// 顶层 — guard / leg 分账要读
		"event":        fmt.Sprintf("%s.loadtest", flow), // 任意非空字符串就行，engine 只用 trigger event_code 路由 edge
		"charge_id":    flowID,
		"amount_minor": amount,
		"currency":     cur,
		"trace_id":     flowID,
		"attributes":   attrs, // ← 关键修正：嵌套，不是摊在顶层
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	// router-only 模式：跳过 accounting（避免账户预创建依赖），只压 split-payment
	// 自身的 translator + router。DryRun 是 stateless 的，需要把 graph 一起塞进去。
	if w.cfg.Load.Mode == "router-only" {
		g := graphCache[graphKey]
		if g == nil {
			return fmt.Errorf("router-only: graph %q not cached (server didn't seed it?)", graphKey)
		}
		dr, err := w.spClient.DryRun(ctx, &spadmin.DryRunRequest{
			Graph:     g,
			EventJson: payload,
		})
		if err != nil {
			return fmt.Errorf("dryrun: %w", err)
		}
		if dr == nil {
			return fmt.Errorf("dryrun: nil response")
		}
		if dr.Error != "" {
			return fmt.Errorf("dryrun business: %s", dr.Error)
		}
		return nil
	}

	req := &spadmin.TriggerEventRequest{
		GraphKey:  graphKey,
		EventJson: payload,
	}
	resp, err := w.spClient.TriggerEvent(ctx, req)
	if err != nil {
		return fmt.Errorf("trigger: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("nil response")
	}
	// 关键：TriggerEventResponse 业务失败时 RPC 层是 success，错误塞在 resp.Error。
	// 不算这层 → loadtest 看到的 errs 永远是 0，TPS 严重虚高。
	if resp.Error != "" {
		return fmt.Errorf("business: %s", resp.Error)
	}
	return nil
}

// ─── main ─────────────────────────────────────────────────────────────────

func main() {
	configPath := flag.String("config", "/loadtest/config.yaml", "path to loadtest yaml config")
	overrideMode := flag.String("mode", "", "override load.mode (topup/payment/transfer/withdraw/mixed/router-only)")
	overrideConc := flag.Int("concurrency", 0, "override load.concurrency")
	overrideDur := flag.Duration("duration", 0, "override load.duration")
	flag.Parse()

	cfg, err := loadConfig0(*configPath)
	if err != nil {
		fatal("load config: %v", err)
	}
	if *overrideMode != "" {
		cfg.Load.Mode = *overrideMode
	}
	if *overrideConc > 0 {
		cfg.Load.Concurrency = *overrideConc
	}
	if *overrideDur > 0 {
		cfg.Load.Duration = *overrideDur
	}
	if cfg.Load.Concurrency <= 0 {
		cfg.Load.Concurrency = 10
	}
	if cfg.Load.Duration <= 0 {
		cfg.Load.Duration = 60 * time.Second
	}

	fmt.Printf("=== loadtest v%s ===\n", buildVersion)
	fmt.Printf("target:        %s (via %s)\n", cfg.Target.Mode, cfg.Target.SplitPaymentEndpoint)
	fmt.Printf("mode:          %s   concurrency: %d   duration: %s\n",
		cfg.Load.Mode, cfg.Load.Concurrency, cfg.Load.Duration)

	// Kitex client → split-payment AdminService
	spClient, err := spadminsvc.NewClient(
		"split-payment",
		client.WithHostPorts(cfg.Target.SplitPaymentEndpoint),
		// 压测 tuning：原来 30s 会让 50 worker 全部卡在慢请求上 30s 不释放，
		// 累计 ops 看起来"涨不动"。5s 让卡住的 worker 快速 fail-fast 释放，下一笔
		// 立刻接上。代价是 errs 会显示更多 timeout 类错，但 TPS 是真实的尝试速率。
		client.WithRPCTimeout(5*time.Second),
	)
	if err != nil {
		fatal("create split-payment client: %v", err)
	}

	// 启动期尝试读 account_pool.json（loadtest-bootstrap 写的真实 account_no 池）。
	// 找不到不算错 —— router-only 模式下没 pool 也能跑（DryRun 不查 accounting），
	// via-split-payment 模式没 pool 会全报 "account not found"，但那个错很清晰，
	// 不需要在 loadtest 启动期硬卡死。
	poolPath := os.Getenv("LOADTEST_ACCOUNT_POOL")
	if poolPath == "" {
		poolPath = "/output/account_pool.json"
	}
	if err := loadAccountPool(poolPath); err != nil {
		fmt.Printf("WARN: account pool 未加载 (%v) — fake 字符串模式（only router-only/DryRun 能跑通）\n", err)
	} else {
		fmt.Printf("account pool: users=%d merchants=%d channels=%d currency=%s ← %s\n",
			len(pool.Users), len(pool.Merchants), len(pool.Channels), pool.Currency, poolPath)
	}

	// 启动期无条件把所有 graph 从盘上读进内存，常驻 graphCache。
	// 用途：
	//   - router-only：dispatch 时塞进 DryRun（stateless 翻译）
	//   - via-split-payment / 单 flow：可做客户端预校验（避免发空 event）+ 排错信息
	//   - 失败 fast-fail：graph 文件缺失/格式错在启动期就报，不到压测中才发现
	// 跟 spClient.GetGraph 比的好处：不依赖 split-payment seed 完成，不引入 RPC race。
	graphDir := os.Getenv("LOADTEST_GRAPH_DIR")
	if graphDir == "" {
		graphDir = "/loadtest/graphs" // 容器内默认 mount 点（compose 配的）
	}
	fmt.Printf("pre-loading graphs from %s ...\n", graphDir)
	if err := loadGraphsFromDir(graphDir, cfg.Flow.GraphKeys); err != nil {
		fatal("loadGraphsFromDir: %v", err)
	}
	fmt.Printf("graphCache: %d graphs ready in memory\n", len(graphCache))

	picker := newWeightedPicker(cfg.Load.MixRatio)

	// 统计对象
	mainStats := newStats(cfg.Report.LatencyBucketsMs)
	flowStats := map[flowKind]*stats{}
	for _, f := range allFlows() {
		flowStats[f] = newStats(cfg.Report.LatencyBucketsMs)
	}

	// 限速器（QPS）
	var limiter <-chan struct{}
	if cfg.Load.RateLimitQPS > 0 {
		c := make(chan struct{}, cfg.Load.RateLimitQPS)
		go func() {
			tick := time.NewTicker(time.Second / time.Duration(cfg.Load.RateLimitQPS))
			defer tick.Stop()
			for range tick.C {
				select {
				case c <- struct{}{}:
				default:
				}
			}
		}()
		limiter = c
	}

	// 上下文
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 信号
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("\n=== interrupt received, stopping... ===")
		cancel()
	}()

	// 预热（warmup 时间不计入统计）
	runDuration := cfg.Load.Duration
	if cfg.Load.Warmup > 0 {
		fmt.Printf("warmup %s ...\n", cfg.Load.Warmup)
		warmupCtx, wcancel := context.WithTimeout(rootCtx, cfg.Load.Warmup)
		runWorkers(warmupCtx, cfg, picker, spClient, limiter,
			newStats(cfg.Report.LatencyBucketsMs),
			discardFlowStats(cfg.Report.LatencyBucketsMs))
		wcancel()
	}

	// 正式压测
	runCtx, rcancel := context.WithTimeout(rootCtx, runDuration)
	defer rcancel()

	// 进度报告 goroutine
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		interval := cfg.Report.ProgressInterval
		if interval <= 0 {
			interval = 5 * time.Second
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		start := time.Now()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				elapsed := time.Since(start)
				ops := mainStats.ops.Load()
				errs := mainStats.errs.Load()
				tps := float64(ops) / elapsed.Seconds()
				fmt.Printf("[%5.1fs] ops=%-8d tps=%-8.1f errs=%-5d p99=%dms\n",
					elapsed.Seconds(), ops, tps, errs, mainStats.percentile(99))
			}
		}
	}()

	startTime := time.Now()
	runWorkers(runCtx, cfg, picker, spClient, limiter, mainStats, flowStats)
	elapsed := time.Since(startTime)
	<-progressDone

	// 汇总报告
	report := buildReport(cfg, elapsed, mainStats, flowStats)
	if err := writeReport(cfg.Report.OutputPath, report); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
	}
	printReportWithErrors(report, mainStats)
}

func runWorkers(
	ctx context.Context,
	cfg *config,
	picker *weightedPicker,
	spClient spadminsvc.Client,
	limiter <-chan struct{},
	mainStats *stats,
	flowStats map[flowKind]*stats,
) {
	var wg sync.WaitGroup
	for i := 0; i < cfg.Load.Concurrency; i++ {
		seed := int64(i)*1_000_003 + time.Now().UnixNano()
		w := &worker{
			id:        i,
			cfg:       cfg,
			picker:    picker,
			spClient:  spClient,
			rng:       mathrand.New(mathrand.NewSource(seed)),
			flowStats: flowStats,
			mainStats: mainStats,
			limiter:   limiter,
		}
		wg.Add(1)
		go w.run(ctx, &wg)
	}
	wg.Wait()
}

func discardFlowStats(buckets []int64) map[flowKind]*stats {
	m := map[flowKind]*stats{}
	for _, f := range allFlows() {
		m[f] = newStats(buckets)
	}
	return m
}

// ─── Report ───────────────────────────────────────────────────────────────

type flowReport struct {
	Ops      int64   `json:"ops"`
	Errs     int64   `json:"errs"`
	TPS      float64 `json:"tps"`
	AvgMs    float64 `json:"avg_ms"`
	P50Ms    int64   `json:"p50_ms"`
	P90Ms    int64   `json:"p90_ms"`
	P95Ms    int64   `json:"p95_ms"`
	P99Ms    int64   `json:"p99_ms"`
}

type fullReport struct {
	BuildVersion string                  `json:"build_version"`
	Mode         string                  `json:"mode"`
	Concurrency  int                     `json:"concurrency"`
	DurationSec  float64                 `json:"duration_sec"`
	TotalOps     int64                   `json:"total_ops"`
	TotalErrs    int64                   `json:"total_errs"`
	ErrRate      float64                 `json:"err_rate"`
	TPS          float64                 `json:"tps"`
	AvgMs        float64                 `json:"avg_ms"`
	P50Ms        int64                   `json:"p50_ms"`
	P90Ms        int64                   `json:"p90_ms"`
	P95Ms        int64                   `json:"p95_ms"`
	P99Ms        int64                   `json:"p99_ms"`
	PerFlow      map[string]flowReport   `json:"per_flow"`
	StartedAt    string                  `json:"started_at"`
	FinishedAt   string                  `json:"finished_at"`
}

func buildReport(cfg *config, elapsed time.Duration, m *stats, per map[flowKind]*stats) fullReport {
	ops := m.ops.Load()
	errs := m.errs.Load()
	rpt := fullReport{
		BuildVersion: buildVersion,
		Mode:         cfg.Load.Mode,
		Concurrency:  cfg.Load.Concurrency,
		DurationSec:  elapsed.Seconds(),
		TotalOps:     ops,
		TotalErrs:    errs,
		ErrRate:      safeRatio(errs, ops),
		TPS:          float64(ops) / elapsed.Seconds(),
		AvgMs:        m.avgLatencyMs(),
		P50Ms:        m.percentile(50),
		P90Ms:        m.percentile(90),
		P95Ms:        m.percentile(95),
		P99Ms:        m.percentile(99),
		PerFlow:      map[string]flowReport{},
		FinishedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	rpt.StartedAt = time.Now().Add(-elapsed).UTC().Format(time.RFC3339)
	for f, s := range per {
		o := s.ops.Load()
		if o == 0 {
			continue
		}
		rpt.PerFlow[string(f)] = flowReport{
			Ops:   o,
			Errs:  s.errs.Load(),
			TPS:   float64(o) / elapsed.Seconds(),
			AvgMs: s.avgLatencyMs(),
			P50Ms: s.percentile(50),
			P90Ms: s.percentile(90),
			P95Ms: s.percentile(95),
			P99Ms: s.percentile(99),
		}
	}
	return rpt
}

// printReportWithErrors 在 fullReport 之上加 errSamples 的 top N 输出。
// 调用方传入 mainStats，因为 errSamples 没序列化进 fullReport。
func printReportWithErrors(r fullReport, mainStats *stats) {
	printReport(r)
	tops := mainStats.topErrors(5)
	if len(tops) == 0 {
		return
	}
	fmt.Println("  Top errors:")
	for _, t := range tops {
		fmt.Printf("    [%d×] %s\n", t.Count, t.Msg)
	}
	fmt.Println("════════════════════════════════════════════════════════════════")
}

func printReport(r fullReport) {
	fmt.Println()
	fmt.Println("════════════════════════════════════════════════════════════════")
	fmt.Printf("  loadtest report — mode=%s duration=%.1fs\n", r.Mode, r.DurationSec)
	fmt.Println("════════════════════════════════════════════════════════════════")
	fmt.Printf("  Total ops:    %d\n", r.TotalOps)
	fmt.Printf("  TPS (avg):    %.1f/sec\n", r.TPS)
	fmt.Printf("  Errors:       %d (%.2f%%)\n", r.TotalErrs, r.ErrRate*100)
	fmt.Println()
	fmt.Println("  Latency (ms):")
	fmt.Printf("    avg:  %.1f\n", r.AvgMs)
	fmt.Printf("    p50:  %d\n", r.P50Ms)
	fmt.Printf("    p90:  %d\n", r.P90Ms)
	fmt.Printf("    p95:  %d\n", r.P95Ms)
	fmt.Printf("    p99:  %d\n", r.P99Ms)
	if len(r.PerFlow) > 0 {
		fmt.Println()
		fmt.Println("  Per-flow breakdown:")
		keys := make([]string, 0, len(r.PerFlow))
		for k := range r.PerFlow {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f := r.PerFlow[k]
			fmt.Printf("    %-10s %7d ops | %6.1f/sec | p99=%dms | errs=%d\n",
				k, f.Ops, f.TPS, f.P99Ms, f.Errs)
		}
	}
	fmt.Println("════════════════════════════════════════════════════════════════")
}

func writeReport(path string, r fullReport) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ─── helpers ──────────────────────────────────────────────────────────────

func loadConfig0(path string) (*config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.AutomaticEnv()
	v.SetEnvPrefix("LOADTEST")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}
	c := &config{}
	if err := v.Unmarshal(c); err != nil {
		return nil, err
	}
	return c, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", args...)
	os.Exit(1)
}

func safeRatio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func maxOr(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func maxOr64(v, fallback int64) int64 {
	if v <= 0 {
		return fallback
	}
	return v
}

// newBizID 生成业务 flow_id。
//
// 限制：accounting 的 transaction_order.order_no 是 VARCHAR(64)。split-payment 在
// translator.go:190 里把 order_no 拼成 `{flow_id}_{event_code}`，其中 event_code
// 最长是 graph 里的 `channel_settled_fee_pending` = 27 chars。所以 flow_id 必须
// ≤ 64 - 1 - 27 = 36 chars，留点 buffer 取 32。
//
// 老格式 `lt-topup-1716266400123456789-deadbeef` = 38 chars + "_channel_settled_fee_pending" 28
// = 66 chars，超 64 一截 → Error 1406 (Data too long for column 'order_no')。
//
// 新格式：`lt{flow_letter}{nanos_hex_8}{rand_hex_8}` = 2 + 1 + 8 + 8 = 19 chars。
// 唯一性：每个 worker 用独立 rand 源，碰撞概率 ≈ 1/2^32 × 同纳秒内多请求，可忽略。
func newBizID(flow string) string {
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	r := binary.BigEndian.Uint32(rb[:])
	// flow 取首字母（t/p/r/w）—— 只用于人工调试时分辨，DB 不依赖
	flowLetter := "x"
	if len(flow) > 0 {
		flowLetter = flow[:1]
	}
	return fmt.Sprintf("lt%s%08x%08x", flowLetter, uint32(time.Now().UnixNano()), r)
}
