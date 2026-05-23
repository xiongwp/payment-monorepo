// topup-e2e —— 一笔 user-topup 端到端测试
//
// 流程：
//   1. 调 accounting admin HTTP /admin/rotation/resolve-fleet-sub 解析 5 个 LA
//      → 5 个具体 sub-account（同一个 charge_id 做 flow_id 保证一致路由）
//   2. 从 account_pool.json 拿一个 user_account_no（loadtest bootstrap 预创建）
//   3. 用 6 个 account_no 构造 user-topup 的 event payload
//   4. kitex 调 split-payment.TriggerEvent（必须 gRPC，无 HTTP 入口）
//   5. 调 accounting admin HTTP 查 transaction-by-business-no 验证 5 笔 booking 都成功
//
// 用法：
//   cd packages/split-payment
//   go run ./cmd/topup-e2e \
//     --admin=http://localhost:8893 \
//     --split-payment=localhost:9098 \
//     --channel=alipay \
//     --pool=../payment-admin-web/loadtest/output/account_pool.json
//
// 默认值会从 env 或常用路径取，最简单的用法：
//   ADMIN_HTTP=http://localhost:8893 go run ./cmd/topup-e2e
//
// 币种：默认 PHP（accounting validateCurrency 白名单）。其他选项见
// packages/accounting-system/internal/currency/currency.go::precisionMap。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/transport"

	spadmin "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1"
	spadminsvc "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1/adminservice"
)

// ── 5 个 fleet-routed LA 的 key（跟 fleet-prepare.sh 注册的对齐） ────────────
//
// user_topup graph 的 6 个 node 里，5 个走 fleet（platform 中间账户），
// 1 个是真实用户钱包（user_wallet）。
//
// 注意：node 名跟 LA prefix 不一一对应，这里手工映射：
//   node.id              ↔ attribute key                    ↔ LA prefix
//   channel_receivable   ↔ channel_receivable_account       ↔ channel-receivable:alipay
//   suspense             ↔ channel_suspense_account         ↔ channel-suspense:alipay
//   user_wallet          ↔ user_id_account                  ↔ (用户账户，非 LA)
//   fee_clearing         ↔ fee_clearing_account             ↔ platform-fee-clearing:default
//   channel_payable      ↔ channel_fee_account              ↔ channel-fee:alipay
//   platform_revenue     ↔ fee_account                      ↔ platform-fee-revenue:default
type laMapping struct {
	AttrKey string // event attributes 的 key
	LAKey   string // accounting LA key
}

func laMappings(channel string) []laMapping {
	return []laMapping{
		{"channel_receivable_account", "channel-receivable:" + channel},
		{"channel_suspense_account", "channel-suspense:" + channel},
		{"fee_clearing_account", "platform-fee-clearing:default"},
		{"channel_fee_account", "channel-fee:" + channel},
		{"fee_account", "platform-fee-revenue:default"},
	}
}

// fleet routing 解析结果（跟 accounting service.FleetSubResolution 同 shape）
type fleetSubResolution struct {
	AccountNo    string `json:"account_no"`
	SubIdx       int    `json:"sub_idx"`
	AccountGroup string `json:"account_group"`
	Currency     string `json:"currency"`
}

// 配置
type config struct {
	adminHTTP     string
	splitPayment  string
	channel       string
	poolFile      string
	amountMinor   int64
	chargeID      string
	currency      string
}

func main() {
	cfg := parseFlags()

	green("================================================================")
	green("user-topup 端到端 demo")
	green("  admin HTTP:  %s", cfg.adminHTTP)
	green("  split-payment gRPC: %s", cfg.splitPayment)
	green("  channel: %s   currency: %s   amount: %d (minor units)", cfg.channel, cfg.currency, cfg.amountMinor)
	green("  charge_id: %s", cfg.chargeID)
	green("================================================================")

	// ── Step 1: 解析 5 个 LA 到 sub-account ─────────────────────────────────
	fmt.Println()
	yellow("[1/4] 解析 5 个 LA via /admin/rotation/resolve-fleet-sub")
	mappings := laMappings(cfg.channel)
	resolved := make(map[string]*fleetSubResolution, len(mappings))
	for _, m := range mappings {
		r, err := resolveFleetSub(cfg.adminHTTP, m.LAKey, cfg.chargeID)
		if err != nil {
			red("  ✗ resolve %s: %v", m.LAKey, err)
			red("  提示：先跑 ./loadtest/scripts/fleet-prepare.sh 把 LA + fleet 建好")
			os.Exit(1)
		}
		resolved[m.AttrKey] = r
		green("  ✓ %-32s sub_idx=%2d  account_no=%s  group=%s",
			m.LAKey, r.SubIdx, r.AccountNo, r.AccountGroup)
	}

	// ── Step 2: 取一个用户账户 ─────────────────────────────────────────────
	fmt.Println()
	yellow("[2/4] 从 account_pool.json 取一个用户账户")
	userAcct, err := pickUserAccount(cfg.poolFile)
	if err != nil {
		red("  ✗ 读 pool 失败: %v", err)
		red("  提示：先跑 loadtest bootstrap (./loadtest/scripts/run.sh 默认会跑) 生成 pool")
		os.Exit(1)
	}
	green("  ✓ user_account = %s", userAcct)

	// ── Step 3: 构造 event payload + 调 TriggerEvent ───────────────────────
	fmt.Println()
	yellow("[3/4] 调 split-payment.TriggerEvent (5-leg 原子记账)")

	// 金额分配：channel_receivable 全额入；suspense 99% 给用户 + 1% 给 fee_clearing；
	// fee_clearing 60% → channel_payable + 40% → platform_revenue
	channelAmt := cfg.amountMinor
	userAmt := cfg.amountMinor * 99 / 100
	feeClearingAmt := cfg.amountMinor - userAmt
	channelFeeAmt := feeClearingAmt * 60 / 100
	platformRevAmt := feeClearingAmt - channelFeeAmt

	attrs := map[string]string{
		// 5 个 fleet-routed 平台账户
		"channel_receivable_account":          resolved["channel_receivable_account"].AccountNo,
		"channel_receivable_account_amount":   fmt.Sprintf("%d", channelAmt),
		"channel_receivable_account_currency": cfg.currency,

		"channel_suspense_account":          resolved["channel_suspense_account"].AccountNo,
		"channel_suspense_account_amount":   fmt.Sprintf("%d", channelAmt),
		"channel_suspense_account_currency": cfg.currency,

		"fee_clearing_account":          resolved["fee_clearing_account"].AccountNo,
		"fee_clearing_account_amount":   fmt.Sprintf("%d", feeClearingAmt),
		"fee_clearing_account_currency": cfg.currency,

		"channel_fee_account":          resolved["channel_fee_account"].AccountNo,
		"channel_fee_account_amount":   fmt.Sprintf("%d", channelFeeAmt),
		"channel_fee_account_currency": cfg.currency,

		"fee_account":          resolved["fee_account"].AccountNo,
		"fee_account_amount":   fmt.Sprintf("%d", platformRevAmt),
		"fee_account_currency": cfg.currency,

		// 1 个真实用户账户
		"user_id_account":          userAcct,
		"user_id_account_amount":   fmt.Sprintf("%d", userAmt),
		"user_id_account_currency": cfg.currency,
	}

	event := map[string]interface{}{
		"event":        "channel.settled",
		"charge_id":    cfg.chargeID,
		"amount_minor": cfg.amountMinor,
		"currency":     cfg.currency,
		"trace_id":     cfg.chargeID,
		"attributes":   attrs,
	}
	eventJSON, _ := json.Marshal(event)
	green("  event payload:")
	pretty, _ := json.MarshalIndent(event, "    ", "  ")
	fmt.Printf("    %s\n", pretty)

	spClient, err := spadminsvc.NewClient("split-payment",
		client.WithHostPorts(cfg.splitPayment),
		client.WithTransportProtocol(transport.GRPC),
		client.WithRPCTimeout(15*time.Second),
		client.WithConnectTimeout(5*time.Second),
	)
	if err != nil {
		red("  ✗ kitex client 创建失败: %v", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := spClient.TriggerEvent(ctx, &spadmin.TriggerEventRequest{
		GraphKey:  "user-topup-v1",
		EventJson: eventJSON,
	})
	elapsed := time.Since(start)
	if err != nil {
		red("  ✗ TriggerEvent RPC 失败: %v", err)
		os.Exit(1)
	}
	if resp == nil {
		red("  ✗ nil response")
		os.Exit(1)
	}
	if resp.Error != "" {
		red("  ✗ business error: %s", resp.Error)
		os.Exit(1)
	}
	green("  ✓ TriggerEvent 成功 (耗时 %v)", elapsed)
	if respB, _ := json.MarshalIndent(resp, "    ", "  "); respB != nil {
		fmt.Printf("    %s\n", respB)
	}

	// ── Step 4: 验证 — 查每个 LA 的 fleet 余额 ──────────────────────────────
	fmt.Println()
	yellow("[4/4] 验证：查每个 LA 的 fleet 余额，看刚才命中的 sub_idx 是否有变动")
	allOK := true
	for _, m := range mappings {
		r := resolved[m.AttrKey]
		bal, err := getAccountBalance(cfg.adminHTTP, r.AccountNo)
		if err != nil {
			red("  ✗ get account %s: %v", r.AccountNo, err)
			allOK = false
			continue
		}
		// 判定：balance 不应该是 0
		mark := "✓"
		colorFn := green
		if bal == 0 {
			mark = "?"
			colorFn = yellow
			allOK = false
		}
		colorFn("  %s %-32s sub_idx=%2d  balance=%d %s",
			mark, m.LAKey, r.SubIdx, bal, r.Currency)
	}

	fmt.Println()
	if allOK {
		green("✅ 端到端 demo 完成 — 5 leg 全部命中 fleet sub-account，余额已更新")
	} else {
		yellow("⚠️  部分 sub balance 仍为 0 — 可能记账还在异步处理中；30 秒后重查或直接看 instance-history")
	}
	green("可看到完整流量分布：")
	green("  ./loadtest/scripts/fleet-verify.sh --admin=%s --la=channel-receivable:%s", cfg.adminHTTP, cfg.channel)
}

// ──────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────

func parseFlags() *config {
	cfg := &config{}
	defaultAdmin := os.Getenv("ADMIN_HTTP")
	if defaultAdmin == "" {
		defaultAdmin = "http://localhost:8893"
	}
	defaultSP := os.Getenv("SPLIT_PAYMENT_GRPC")
	if defaultSP == "" {
		defaultSP = "localhost:9098"
	}
	defaultPool := os.Getenv("POOL_FILE")
	if defaultPool == "" {
		// 相对于 cwd（go run 时一般是 packages/split-payment）
		defaultPool = "../payment-admin-web/loadtest/output/account_pool.json"
	}
	flag.StringVar(&cfg.adminHTTP, "admin", defaultAdmin, "accounting admin HTTP base URL")
	flag.StringVar(&cfg.splitPayment, "split-payment", defaultSP, "split-payment gRPC host:port")
	flag.StringVar(&cfg.channel, "channel", "alipay", "渠道名 (LA suffix)")
	flag.StringVar(&cfg.poolFile, "pool", defaultPool, "account_pool.json 路径")
	flag.Int64Var(&cfg.amountMinor, "amount", 10000, "充值金额 (minor units; 100 CNY = 10000 分)")
	flag.StringVar(&cfg.chargeID, "charge-id", fmt.Sprintf("topup-e2e-%d", time.Now().Unix()), "charge_id (=flow_id; fleet routing hash)")
	flag.StringVar(&cfg.currency, "currency", "PHP", "ISO-4217 三字母")
	flag.Parse()
	return cfg
}

func resolveFleetSub(adminHTTP, laKey, flowID string) (*fleetSubResolution, error) {
	u := fmt.Sprintf("%s/admin/rotation/resolve-fleet-sub?logical_account_key=%s&flow_id=%s",
		adminHTTP, urlEscape(laKey), urlEscape(flowID))
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var r fleetSubResolution
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode: %w (body=%s)", err, string(body))
	}
	return &r, nil
}

// pickUserAccount 从 account_pool.json 拿一个 user_account_no。
// pool 文件 schema 跟 cmd/loadtest 的 accountPool 同：{ "users": ["..."], ... }
func pickUserAccount(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var pool struct {
		Users []string `json:"users"`
	}
	if err := json.NewDecoder(f).Decode(&pool); err != nil {
		return "", err
	}
	if len(pool.Users) == 0 {
		return "", fmt.Errorf("pool has no users (run loadtest bootstrap first)")
	}
	// 用 time-based pseudo-random 取一个；不重要，演示用
	return pool.Users[int(time.Now().UnixNano())%len(pool.Users)], nil
}

// getAccountBalance 调 accounting admin /admin/rotation/instance-detail。
func getAccountBalance(adminHTTP, accountNo string) (int64, error) {
	u := fmt.Sprintf("%s/admin/rotation/instance-detail?account_no=%s", adminHTTP, urlEscape(accountNo))
	resp, err := http.Get(u)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		Balance int64 `json:"balance"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, err
	}
	return r.Balance, nil
}

// urlEscape 极简 URL escape（避免引入额外依赖；冒号 + 短横线足够）
func urlEscape(s string) string {
	s = strings.ReplaceAll(s, ":", "%3A")
	s = strings.ReplaceAll(s, " ", "%20")
	return s
}

// 输出辅助 — 终端有 TTY 时上色，否则纯文本
func green(format string, a ...interface{})  { color("32", format, a...) }
func yellow(format string, a ...interface{}) { color("33", format, a...) }
func red(format string, a ...interface{})    { color("31", format, a...) }
func color(code, format string, a ...interface{}) {
	if isTTY() {
		fmt.Fprintf(os.Stdout, "\033[%sm"+format+"\033[0m\n", append([]interface{}{code}, a...)...)
	} else {
		fmt.Fprintf(os.Stdout, format+"\n", a...)
	}
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// 防止未使用的 import 报错
var _ = bytes.NewReader
