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
		return flowKind("router-only")
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

	event := map[string]any{
		"flow_id":      flowID,
		"requested_at": time.Now().UTC().Format(time.RFC3339Nano),
	}

	// 每个 graph 的字段集
	switch flow {
	case flowTopup:
		// graph topup.json: channel_receivable → suspense → user_wallet, fee_clearing → channel_payable, platform_revenue
		net := amount - 100 // 留 100 分手续费
		if net < 1 {
			net = 1
		}
		event["channel_receivable_account"], event["channel_receivable_account_amount"], event["channel_receivable_account_currency"] = fmt.Sprintf("ch-%d/recv", channelID), amtStr, cur
		event["channel_suspense_account"], event["channel_suspense_account_amount"], event["channel_suspense_account_currency"] = fmt.Sprintf("ch-%d/suspense", channelID), amtStr, cur
		event["user_id_account"], event["user_id_account_amount"], event["user_id_account_currency"] = fmt.Sprintf("%d", userID), fmt.Sprintf("%d", net), cur
		event["fee_clearing_account"], event["fee_clearing_account_amount"], event["fee_clearing_account_currency"] = "platform/fee_clearing", "100", cur
		event["channel_fee_account"], event["channel_fee_account_amount"], event["channel_fee_account_currency"] = fmt.Sprintf("ch-%d/fee", channelID), "60", cur
		event["fee_account"], event["fee_account_amount"], event["fee_account_currency"] = "platform/fee_revenue", "40", cur

	case flowPayment:
		// graph payment.json: user_wallet → merchant_pending → merchant_wallet
		merchantID := w.rng.Intn(100) + 1
		event["payer_account"], event["payer_account_amount"], event["payer_account_currency"] = fmt.Sprintf("%d", userID), amtStr, cur
		event["merchant_pending_account"], event["merchant_pending_account_amount"], event["merchant_pending_account_currency"] = fmt.Sprintf("m-%d/pending", merchantID), amtStr, cur
		event["merchant_account"], event["merchant_account_amount"], event["merchant_account_currency"] = fmt.Sprintf("m-%d", merchantID), amtStr, cur

	case flowTransfer:
		// graph transfer.json: from_wallet → to_wallet
		event["from_account"], event["from_account_amount"], event["from_account_currency"] = fmt.Sprintf("%d", userID), amtStr, cur
		event["to_account"], event["to_account_amount"], event["to_account_currency"] = fmt.Sprintf("%d", peerUserID), amtStr, cur

	case flowWithdraw:
		// graph withdraw.json: user_wallet → withdraw_pending → channel_payable
		event["user_account"], event["user_account_amount"], event["user_account_currency"] = fmt.Sprintf("%d", userID), amtStr, cur
		event["withdraw_pending_account"], event["withdraw_pending_account_amount"], event["withdraw_pending_account_currency"] = "platform/withdraw_pending", amtStr, cur
		event["channel_payable_account"], event["channel_payable_account_amount"], event["channel_payable_account_currency"] = fmt.Sprintf("ch-%d/payable", channelID), amtStr, cur
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
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
		client.WithRPCTimeout(30*time.Second),
	)
	if err != nil {
		fatal("create split-payment client: %v", err)
	}

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

// newBizID 生成业务 flow_id：lt-{flowType}-{ts}{rand4}
// 注意：业务 flow_id 必须保证全局唯一，由调用方负责；这里用 ns + 32bit rand 已满足 1M/sec。
func newBizID(flow string) string {
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	r := binary.BigEndian.Uint32(rb[:])
	return fmt.Sprintf("lt-%s-%d-%08x", flow, time.Now().UnixNano(), r)
}
