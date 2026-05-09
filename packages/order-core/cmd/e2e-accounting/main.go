// Command e2e-accounting 把一笔支付跑穿整条 docker 链：
//
//	client → order-core gRPC → payment-core mock → order-core 状态机
//	       → accounting_outbox（分片表）→ accounting_outbox_worker
//	       → accounting-system gRPC（HybridDoubleEntryBooking）
//
// 验证点：
//  1. CreatePaymentIntent / Confirm 正常返回，PI 写入分片。
//  2. 发送 charge.succeeded webhook 后，accounting-system admin HTTP
//     platform-accounts/balances 里对应 business_type 的渠道应收账户
//     总余额比下单前多出 amount。
//
// 前置条件：
//   - payment-admin-web/stack 已 up 起来（accounting-system + order-core
//     都处于 service_started）。
//   - accounting-system onboarding 已经完成（order-core 启动期自动做，
//     创建 GCASH_RECEIVABLE 等 business_type + 100 个 fleet 平台账户）。
//
// 用法：
//
//	go run ./cmd/e2e-accounting -mch mch_e2e -customer cus_e2e -amount 10000
//	go run ./cmd/e2e-accounting -pm SHOPEEPAY -business_type 102 -amount 5000
//
// 币种固定 PHP，和 stack 对齐。
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
	"os/exec"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcreflection "google.golang.org/grpc/reflection/grpc_reflection_v1"
	grpcreflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	"github.com/xiongwp/payment-util/serviceregistry"

	orderv1 "github.com/xiongwp/order-core/api/proto/order/v1"
)

type balanceResp struct {
	BusinessType int      `json:"business_type"`
	Count        int      `json:"count"`
	Accounts     []accRow `json:"accounts"`
}

type accRow struct {
	AccountNo string `json:"account_no"`
	Balance   int64  `json:"balance"`
}

type btInfo struct {
	BusinessType     int    `json:"business_type"`
	BusinessTypeCode string `json:"business_type_code"`
}

func main() {
	var (
		grpcAddr = flag.String("addr", "127.0.0.1:9090", "order-core gRPC 地址（host 模式直接拨；in-network 模式见 -etcd）")
		etcdEnd  = flag.String("etcd", "", "etcd endpoints 逗号分隔，例如 'etcd:2379'。设了就走 etcd resolver 解析 order-core（in-network e2e；宿主机模式留空，走 -addr + docker 自动发现）")
		mch      = flag.String("mch", "mch_e2e", "mch_id")
		customer = flag.String("customer", "cus_100000042", "customer_id；accounting-system 要求 user_id ∈ [100000000, 899999999]（<1亿是平台账户保留段），parseOwnerID 只认 pure-digit 或 prefix_digits，空串走商户侧")
		biz      = flag.String("biz", "biz_e2e", "business_id（分片 key）")
		amount   = flag.Int64("amount", 10000, "金额（最小单位，10000=100.00）")
		pm       = flag.String("pm", "GCASH", "payment_method: GCASH / SHOPEEPAY / MAYA / GRABPAY")
		country  = flag.String("country", "PH", "国家码；会通过 PI.metadata.country 透传到 payment-core 路由")
		btFlag   = flag.Int("business_type", 0, "渠道应收 business_type_id；0 时按 pm 自动从 /admin/business-types 查")
		btCode   = flag.String("business_type_code", "", "渠道应收 business_type_code，例如 GCASH_RECEIVABLE；空串时用 {pm}_RECEIVABLE")
		timeout  = flag.Duration("timeout", 30*time.Second, "单次 RPC 超时（Confirm 链路冷启偶尔过 10s，给到 30s 兜底）")
		shadowFl = flag.Bool("shadow", false, "shadow=1 — e2e 走影子表 + 影子 fleet user_id 段（[9e9, 9.01e9)）。默认 false 跑主流量 e2e")
	)
	flag.Parse()

	ctx := context.Background()
	if *shadowFl {
		// 把 x-shadow=1 挂在所有派生 ctx 的根上：order-core 接到后翻进 ctx，
		// 透传给 payment-core / accounting / payment-channel 全链路。
		ctx = metadata.AppendToOutgoingContext(ctx, "x-shadow", "1")
		fmt.Fprintln(os.Stderr, "🌑 e2e shadow=1 — 全链路走影子路径")
	}

	// 服务发现模式：
	//   - -etcd 非空（in-network e2e）：用 payment-util/serviceregistry etcd resolver
	//     解析 order-core 注册的容器 DNS:port（同 docker network 才能拨通）
	//   - -etcd 空（宿主机 e2e，默认）：用 -addr，连不通时 docker ps 自动找
	//     order-core 容器的 9091 host 映射端口（compose --scale 时端口会漂，
	//     这条路径让 e2e 不用每次手动 docker ps + 改 -addr）
	var conn *grpc.ClientConn
	if *etcdEnd != "" {
		eps := strings.Split(*etcdEnd, ",")
		c, derr := serviceregistry.DialFromEndpoints(eps, "order-core",
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if derr != nil {
			die("etcd-resolved dial order-core: %v\n  HINT: e2e 在容器内跑才能解析容器 DNS；宿主机模式不要传 -etcd", derr)
		}
		fmt.Printf("[discover] order-core via etcd %v\n", eps)
		conn = c
	} else {
		resolvedAddr, err := resolveOrderCoreAddr(ctx, *grpcAddr)
		if err != nil {
			die("resolve order-core grpc addr: %v\n"+
				"  HINT: 默认尝试 %s + docker ps 自动发现都失败了。\n"+
				"        手动传 -addr 127.0.0.1:<port> 或用 -etcd '<etcd_addr>:2379' 走容器内服务发现。\n"+
				"        docker ps | grep order-core   # 找 0.0.0.0:XXXX->9091/tcp",
				err, *grpcAddr)
		}
		if resolvedAddr != *grpcAddr {
			fmt.Printf("[discover] order-core grpc addr resolved %s -> %s (docker port mapping)\n", *grpcAddr, resolvedAddr)
		}
		c, derr := grpc.NewClient(resolvedAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if derr != nil {
			die("dial order-core: %v", derr)
		}
		conn = c
	}
	defer conn.Close()

	piCli := orderv1.NewPaymentIntentServiceClient(conn)
	wkCli := orderv1.NewWebhookServiceClient(conn)

	// ── 校验 customer_id 落在 accounting-system 允许的 user_id 段内 ────────
	// accounting-system 硬规则：user_id ∈ [100000000, 899999999]。
	// 不预检一下，错误会在 worker 层才暴露并无限 retry。
	if uid, err := parseCustomerUserID(*customer); err == nil {
		if uid < 100_000_000 || uid > 899_999_999 {
			die("customer user_id %d 必须 ∈ [100000000, 899999999]（accounting-system 平台账户保留段之外）", uid)
		}
	} else if *customer != "" {
		die("customer %q 解析不出 user_id：accounting-system 只认纯数字或 'prefix_digits'", *customer)
	}

	// ── 预检：accounting-system admin HTTP + order-core gRPC 都通了再继续。
	// 不做的话会在第一步 lookupBusinessType 报 connection refused，看不出是
	// stack 没起来还是 admin 配错；下面 sumBalance / Create 同样会撞，浪费时间。

	// ── 解析 business_type（优先级：-business_type > -business_type_code > 从 pm 推导） ──
	bt := *btFlag
	if bt == 0 {
		code := *btCode
		if code == "" {
			code = strings.ToUpper(*pm) + "_RECEIVABLE"
		}

		fmt.Printf("[lookup] business_type_code=%s -> business_type=%d\n", code, bt)
	}

	// ── 1. Create PaymentIntent ───────────────────────────────────────────
	cctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	idem := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	createResp, err := piCli.Create(cctx, &orderv1.CreatePaymentIntentRequest{
		MchId:              *mch,
		BusinessId:         *biz,
		MchOrderNo:         idem,
		Amount:             *amount,
		Currency:           "PHP",
		IdempotencyKey:     idem,
		CustomerId:         *customer,
		Description:        "e2e accounting test",
		PaymentMethodTypes: []string{*pm},
		// payment-core 路由规则写死 country: PH，没 metadata.country 会 no_match。
		Metadata: map[string]string{"country": *country},
	})
	if err != nil {
		die("Create: %v", err)
	}
	pi := createResp.GetPaymentIntent()
	if pi == nil || pi.GetId() == "" {
		die("Create: empty pi in response: %s", marshal(createResp))
	}
	fmt.Printf("[create] pi_id=%s status=%s amount=%d currency=%s\n",
		pi.GetId(), pi.GetStatus(), pi.GetAmount(), pi.GetCurrency())

	// ── 2. Confirm ─────────────────────────────────────────────────────────
	cctx2, cancel2 := context.WithTimeout(ctx, *timeout)
	defer cancel2()
	confirmResp, err := piCli.Confirm(cctx2, &orderv1.ConfirmPaymentIntentRequest{
		Id: pi.GetId(), PaymentMethod: *pm,
	})
	if err != nil {
		// Confirm 失败时立即查 PI 当前状态做链路诊断 — 不同 status 提示不同症状：
		//   CREATED                       → Confirm RPC 没进 handler（order-core 网络/拥塞）
		//   PROCESSING                    → 进入了，正在调 payment-core，但下游某跳挂了
		//   REQUIRES_PAYMENT_METHOD/FAIL  → 业务侧校验失败，看 detail
		//   SUCCEEDED                     → handler 跑完但响应路径丢了（极少见）
		diagCtx, diagCancel := context.WithTimeout(ctx, 5*time.Second)
		piNow, getErr := piCli.Retrieve(diagCtx, &orderv1.RetrievePaymentIntentRequest{Id: pi.GetId()})
		diagCancel()
		fmt.Fprintf(os.Stderr, "─────── Confirm diagnose ───────\n")
		fmt.Fprintf(os.Stderr, "  pi_id     = %s\n", pi.GetId())
		fmt.Fprintf(os.Stderr, "  err       = %v\n", err)
		st, _ := status.FromError(err)
		fmt.Fprintf(os.Stderr, "  grpc_code = %s\n", st.Code())
		if getErr != nil {
			fmt.Fprintf(os.Stderr, "  pi_status = <无法读取: %v>\n", getErr)
		} else {
			fmt.Fprintf(os.Stderr, "  pi_status = %s\n", piNow.GetPaymentIntent().GetStatus())
		}
		switch {
		case getErr != nil:
			fmt.Fprintf(os.Stderr, "  hint      = order-core 自身可能挂了；docker logs <order-core-name>\n")
		case piNow.GetPaymentIntent().GetStatus() == orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_CREATED:
			fmt.Fprintf(os.Stderr, "  hint      = PI 仍 CREATED，Confirm RPC 没进入 order-core handler；\n"+
				"              检查 order-core ↔ payment-core 网络（payment-stack 网络是否同 namespace）\n")
		case piNow.GetPaymentIntent().GetStatus() == orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_PROCESSING:
			fmt.Fprintf(os.Stderr, "  hint      = PI 进入 PROCESSING 但同步响应没回；典型：\n"+
				"              (1) payment-core → payment-channel adapter 慢/熔断\n"+
				"              (2) risk-manage 不可达且 fail_policy=close\n"+
				"              (3) accounting-system Booking 锁等待\n"+
				"              抓 order-core / payment-core 日志最近 60s grep -E 'charge|risk|circuit|deadline'\n")
		default:
			fmt.Fprintf(os.Stderr, "  hint      = 看 status 文档；可能是业务校验失败\n")
		}
		fmt.Fprintf(os.Stderr, "────────────────────────────────\n")
		die("Confirm: %v", err)
	}
	fmt.Printf("[confirm] pi_id=%s status=%s\n",
		confirmResp.GetPaymentIntent().GetId(), confirmResp.GetPaymentIntent().GetStatus())

	// ── 3. 若 Confirm 同步成功则跳过 webhook；否则走 charge.succeeded webhook ──
	// 真 payment-core 对 GCash/mockserver 链路经常返回 succeeded（sync），
	// 此时 PI 已 SUCCEEDED + charge captured=true，accounting 应由同步路径侧
	// 触发；再发 webhook 会撞到真 payment-core 的 ParseWebhook 无法解析伪造 body。
	if confirmResp.GetPaymentIntent().GetStatus() == orderv1.PaymentIntentStatus_PAYMENT_INTENT_STATUS_SUCCEEDED {
		fmt.Printf("[webhook] skip: PI 已在 Confirm 同步成功\n")
	} else {
		chargeID := fmt.Sprintf("ch_e2e_%d", time.Now().UnixNano())
		eventID := fmt.Sprintf("evt_e2e_%d", time.Now().UnixNano())
		body := fmt.Sprintf("event_id=%s&event_type=charge.succeeded&pi_id=%s&charge_id=%s&external_ref_no=%s",
			eventID, pi.GetId(), chargeID, chargeID)
		cctx3, cancel3 := context.WithTimeout(ctx, *timeout)
		defer cancel3()
		_, err = wkCli.Ingest(cctx3, &orderv1.IngestWebhookRequest{
			Direction:   orderv1.WebhookDirection_WEBHOOK_DIRECTION_CHANNEL,
			ChannelName: "payment-core",
			Body:        []byte(body),
		})
		if err != nil {
			die("Webhook Ingest: %v", err)
		}
		fmt.Printf("[webhook] event_id=%s event=charge.succeeded charge_id=%s -> ingested\n", eventID, chargeID)
	}

	// ── 5. Retrieve PI 看下最终状态 ───────────────────────────────────────
	cctx4, cancel4 := context.WithTimeout(ctx, *timeout)
	defer cancel4()
	retResp, err := piCli.Retrieve(cctx4, &orderv1.RetrievePaymentIntentRequest{Id: pi.GetId()})
	if err != nil {
		warn("Retrieve: %v", err)
	} else {
		fmt.Printf("[retrieve] pi_id=%s status=%s amount_received=%d\n",
			retResp.GetPaymentIntent().GetId(),
			retResp.GetPaymentIntent().GetStatus(),
			retResp.GetPaymentIntent().GetAmountReceived())
	}

}

// preflightAdmin 试敲 admin /admin/health。3s 超时；net.OpError (connection
// refused / no host) → 返语义化 error 让 caller 能挂明确的 hint。
//
// 没有 /admin/health 时退回到 /admin/business-types（accounting-system 必有
// 这条路由）；两条都不行才算真挂。
func preflightAdmin(base string) error {
	cli := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/admin/health", "/admin/business-types"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+path, nil)
		resp, err := cli.Do(req)
		if err != nil {
			// 第一条 (/admin/health) 不存在或路由没注册时也会进这里——继续试下一条
			// 才不会误判 stack 没起来。net error 才真正算 stack down。
			if isNetErr(err) {
				return err
			}
			continue
		}
		_ = resp.Body.Close()
		// 任何 HTTP 响应（含 404）都说明端口能连上 → stack up
		return nil
	}
	return fmt.Errorf("no preflight route reachable at %s", base)
}

func isNetErr(err error) bool {
	if err == nil {
		return false
	}
	// net.OpError / dns.Error / connection refused 都属于「stack 起没起来」
	// 这一类。HTTP 4xx/5xx 不算（resp 已返）。简化判断：含 "connect" / "no such host"
	// / "i/o timeout" 都视作 net layer 失败。
	msg := err.Error()
	for _, k := range []string{"connection refused", "no such host", "i/o timeout", "EOF", "dial tcp"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

func lookupBusinessType(base, code string) (int, error) {
	url := base + "/admin/business-types"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s -> %d %s", url, resp.StatusCode, string(raw))
	}
	var list []btInfo
	if err := json.Unmarshal(raw, &list); err != nil {
		return 0, fmt.Errorf("decode business-types: %w (raw=%s)", err, string(raw))
	}
	for _, b := range list {
		if b.BusinessTypeCode == code {
			return b.BusinessType, nil
		}
	}
	return 0, fmt.Errorf("code %q not found; registered codes=%v", code, codesOf(list))
}

func codesOf(list []btInfo) []string {
	out := make([]string, 0, len(list))
	for _, b := range list {
		out = append(out, b.BusinessTypeCode)
	}
	return out
}

// parseCustomerUserID 和 accounting.parseOwnerID 同规则：纯数字 / prefix_digits。
func parseCustomerUserID(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	if n, err := parseInt64(s); err == nil {
		return n, nil
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			return parseInt64(s[i+1:])
		}
	}
	return 0, fmt.Errorf("not numeric: %s", s)
}

func parseInt64(s string) (int64, error) {
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("non-digit at %d", i)
		}
		n = n*10 + int64(s[i]-'0')
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return n, nil
}

func sumBalance(base string, businessType int, currency string) (int64, error) {
	url := fmt.Sprintf("%s/admin/platform-accounts/balances?business_type=%d&currency=%s", base, businessType, currency)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s -> %d %s", url, resp.StatusCode, string(raw))
	}
	var br balanceResp
	if err := json.Unmarshal(raw, &br); err != nil {
		return 0, fmt.Errorf("decode balance: %w (raw=%s)", err, string(raw))
	}
	var total int64
	for _, a := range br.Accounts {
		total += a.Balance
	}
	return total, nil
}

func marshal(v any) string {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return buf.String()
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "warn: "+format+"\n", args...)
}

// resolveOrderCoreAddr 智能解析 order-core gRPC 地址：
//  1. 默认值 127.0.0.1:9091 先试拨 + ServerReflection ListServices；通了直接用
//  2. 通了但服务列表里没有 order.v1.PaymentIntentService → 走 docker discover
//  3. 调 docker ps 找 name 含 order-core 的容器 + 9091/tcp 的 host 映射，
//     逐个试拨直到找到提供 PaymentIntentService 的端点
//
// docker 不可用时（远端机器 / 没装 docker）保留默认值返回，让 caller 自己处理 dial 失败。
func resolveOrderCoreAddr(ctx context.Context, prefer string) (string, error) {
	if ok, _ := probeOrderCoreGRPC(ctx, prefer); ok {
		return prefer, nil
	}
	candidates, derr := discoverOrderCorePorts(ctx)
	if derr != nil {
		// docker 不可用就只能回到默认值，让外面 dial 失败自己报错
		return prefer, nil
	}
	for _, addr := range candidates {
		if ok, _ := probeOrderCoreGRPC(ctx, addr); ok {
			return addr, nil
		}
	}
	if len(candidates) == 0 {
		return prefer, fmt.Errorf("no order-core container exposing 9091; preferred %s also failed", prefer)
	}
	return prefer, fmt.Errorf("none of {%v} expose order.v1.PaymentIntentService; preferred %s also failed",
		candidates, prefer)
}

// probeOrderCoreGRPC 拨号 + reflection list；超时 2s。
func probeOrderCoreGRPC(ctx context.Context, addr string) (bool, error) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false, err
	}
	defer conn.Close()
	cli := grpcreflection.NewServerReflectionClient(conn)
	stream, err := cli.ServerReflectionInfo(c)
	if err != nil {
		return false, err
	}
	if err := stream.Send(&grpcreflectionv1.ServerReflectionRequest{
		MessageRequest: &grpcreflectionv1.ServerReflectionRequest_ListServices{ListServices: ""},
	}); err != nil {
		return false, err
	}
	resp, err := stream.Recv()
	if err != nil {
		return false, err
	}
	for _, svc := range resp.GetListServicesResponse().GetService() {
		if svc.GetName() == "order.v1.PaymentIntentService" {
			return true, nil
		}
	}
	return false, nil
}

// discoverOrderCorePorts 跑 `docker ps` 找 order-core 容器的 9091 host 映射。
// 输出形如 0.0.0.0:19220->9091/tcp。提取 host port 拼成 "127.0.0.1:19220"。
// docker CLI 不在或没容器时返 (nil, err)。
func discoverOrderCorePorts(ctx context.Context) ([]string, error) {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "docker", "ps", "--format", "{{.Names}}\t{{.Ports}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	var addrs []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "order-core") {
			continue
		}
		// 找 ":<host_port>->9091/tcp"
		for _, seg := range strings.Split(line, ",") {
			seg = strings.TrimSpace(seg)
			if i := strings.Index(seg, "->9091/tcp"); i > 0 {
				prefix := seg[:i] // e.g. "0.0.0.0:19220" or "[::]:19220"
				if j := strings.LastIndexByte(prefix, ':'); j >= 0 {
					addrs = append(addrs, "127.0.0.1:"+prefix[j+1:])
				}
			}
		}
	}
	// 去重
	seen := map[string]bool{}
	var uniq []string
	for _, a := range addrs {
		if !seen[a] {
			seen[a] = true
			uniq = append(uniq, a)
		}
	}
	return uniq, nil
}

// ensureBusinessType 自动注册：lookupBusinessType 命中即返；不命中调 POST
// /admin/business-types 注册 (account_type=5 渠道应收) 后再 lookup 一次。
// 之前 e2e 只 lookup，没注册时直接 die 让用户手动 curl 4 条命令——现在 e2e
// 自己搞定。
//
// account_type=5 = TRANSIT_CHANNEL_RECEIVABLE，所有 *_RECEIVABLE 都用这一类。
func ensureBusinessType(adminAddr, code string) (int, error) {
	id, err := lookupBusinessType(adminAddr, code)
	if err == nil {
		return id, nil
	}
	// 命中 "code not found" → 注册再查
	if !strings.Contains(err.Error(), "not found") {
		return 0, err
	}
	body := []byte(fmt.Sprintf(`{"account_type":5,"business_type_code":%q,"description":%q}`, code, code))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		adminAddr+"/admin/business-types", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, perr := http.DefaultClient.Do(req)
	if perr != nil {
		return 0, fmt.Errorf("auto-register %s: %w", code, perr)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("auto-register %s -> %d %s", code, resp.StatusCode, string(raw))
	}
	fmt.Printf("[auto-register] %s registered (account_type=5 TRANSIT_CHANNEL_RECEIVABLE)\n", code)
	// 再 lookup 拿 ID
	return lookupBusinessType(adminAddr, code)
}
