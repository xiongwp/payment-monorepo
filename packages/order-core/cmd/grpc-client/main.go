// Command grpc-client 是 order-core 的 CLI 调试工具。
//
// 用法：
//
//	grpc-client -addr 127.0.0.1:9091 create -mch m1 -biz biz1 -amount 1000 -currency USD
//	grpc-client -addr 127.0.0.1:9091 confirm -id pi_xxxx -pm VISA
//	grpc-client -addr 127.0.0.1:9091 confirm-action -id pi_xxxx -action act_xxx -k otp=123456
//	grpc-client -addr 127.0.0.1:9091 capture -id pi_xxxx
//	grpc-client -addr 127.0.0.1:9091 cancel -id pi_xxxx -reason fraud
//	grpc-client -addr 127.0.0.1:9091 retrieve -id pi_xxxx
//	grpc-client -addr 127.0.0.1:9091 list -mch m1
//	grpc-client -addr 127.0.0.1:9091 refund -pi pi_xxx -amount 500
//	grpc-client -addr 127.0.0.1:9091 webhook -channel payment-core -event_id e1 -event charge.succeeded -pi pi_xxx -charge ch_xxx
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	orderv1 "github.com/xiongwp/order-core/api/proto/order/v1"
)

type kvFlag map[string]string

func (k *kvFlag) String() string { return fmt.Sprintf("%v", *k) }
func (k *kvFlag) Set(s string) error {
	if *k == nil {
		*k = map[string]string{}
	}
	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("expected key=value, got %q", s)
	}
	(*k)[parts[0]] = parts[1]
	return nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9091", "gRPC server address")
	token := flag.String("token", "", "Bearer token (if auth enabled on server)")
	timeout := flag.Duration("timeout", 5*time.Second, "rpc timeout")
	shadowFlag := flag.Bool("shadow", false, "Send as shadow traffic (x-shadow=1 metadata; routes to _shadow tables on server)")
	traceID := flag.String("trace_id", "", "Override traceparent x-trace-id (debug only; default = auto)")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: grpc-client [-addr ...] [-shadow] [-trace_id ID] <command> [flags]")
		os.Exit(2)
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		die(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if *token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+*token)
	}
	if *shadowFlag {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-shadow", "1")
		fmt.Fprintln(os.Stderr, "[grpc-client] shadow=1 — server will route to _shadow tables")
	}
	if *traceID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-trace-id", *traceID)
	}

	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "create":
		runCreate(ctx, conn, rest)
	case "confirm":
		runConfirm(ctx, conn, rest)
	case "confirm-action":
		runConfirmAction(ctx, conn, rest)
	case "capture":
		runCapture(ctx, conn, rest)
	case "cancel":
		runCancel(ctx, conn, rest)
	case "retrieve":
		runRetrieve(ctx, conn, rest)
	case "list":
		runList(ctx, conn, rest)
	case "refund":
		runRefund(ctx, conn, rest)
	case "list-charges":
		runListCharges(ctx, conn, rest)
	case "list-refunds":
		runListRefunds(ctx, conn, rest)
	case "webhook":
		runWebhook(ctx, conn, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(2)
	}
}

func runCreate(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	mch := fs.String("mch", "", "mch_id")
	biz := fs.String("biz", "", "business_id (shard key)")
	mchOrder := fs.String("mch_order_no", "", "mch_order_no")
	amount := fs.Int64("amount", 0, "amount")
	currency := fs.String("currency", "USD", "currency")
	idem := fs.String("idem", "", "idempotency_key")
	customer := fs.String("customer", "", "customer_id")
	desc := fs.String("desc", "", "description")
	notify := fs.String("notify_url", "", "notify_url")
	ret := fs.String("return_url", "", "return_url")
	pmts := fs.String("pmts", "", "comma-separated payment_method_types: VISA,BALANCE")
	live := fs.Bool("live", false, "livemode")
	_ = fs.Parse(args)

	cli := orderv1.NewPaymentIntentServiceClient(conn)
	resp, err := cli.Create(ctx, &orderv1.CreatePaymentIntentRequest{
		MchId:              *mch,
		BusinessId:         *biz,
		MchOrderNo:         *mchOrder,
		Amount:             *amount,
		Currency:           *currency,
		IdempotencyKey:     *idem,
		CustomerId:         *customer,
		Description:        *desc,
		NotifyUrl:          *notify,
		ReturnUrl:          *ret,
		PaymentMethodTypes: csv(*pmts),
		Livemode:           *live,
	})
	dump(resp, err)
}

func runConfirm(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("confirm", flag.ExitOnError)
	id := fs.String("id", "", "pi_id")
	pm := fs.String("pm", "", "payment_method (single)")
	cs := fs.String("cs", "", "client_secret")
	_ = fs.Parse(args)
	cli := orderv1.NewPaymentIntentServiceClient(conn)
	resp, err := cli.Confirm(ctx, &orderv1.ConfirmPaymentIntentRequest{
		Id: *id, PaymentMethod: *pm, ClientSecret: *cs,
	})
	dump(resp, err)
}

func runConfirmAction(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("confirm-action", flag.ExitOnError)
	id := fs.String("id", "", "pi_id")
	actionID := fs.String("action", "", "action_id")
	var data kvFlag
	fs.Var(&data, "k", "verification_data key=value (repeatable)")
	_ = fs.Parse(args)
	cli := orderv1.NewPaymentIntentServiceClient(conn)
	resp, err := cli.ConfirmPaymentAction(ctx, &orderv1.ConfirmPaymentActionRequest{
		Id: *id, ActionId: *actionID, VerificationData: data,
	})
	dump(resp, err)
}

func runCapture(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	id := fs.String("id", "", "pi_id")
	amt := fs.Int64("amount", 0, "amount_to_capture (0 = full)")
	_ = fs.Parse(args)
	cli := orderv1.NewPaymentIntentServiceClient(conn)
	resp, err := cli.Capture(ctx, &orderv1.CapturePaymentIntentRequest{
		Id: *id, AmountToCapture: *amt,
	})
	dump(resp, err)
}

func runCancel(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	id := fs.String("id", "", "pi_id")
	reason := fs.String("reason", "", "cancellation_reason")
	_ = fs.Parse(args)
	cli := orderv1.NewPaymentIntentServiceClient(conn)
	resp, err := cli.Cancel(ctx, &orderv1.CancelPaymentIntentRequest{
		Id: *id, CancellationReason: *reason,
	})
	dump(resp, err)
}

func runRetrieve(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("retrieve", flag.ExitOnError)
	id := fs.String("id", "", "pi_id")
	_ = fs.Parse(args)
	cli := orderv1.NewPaymentIntentServiceClient(conn)
	resp, err := cli.Retrieve(ctx, &orderv1.RetrievePaymentIntentRequest{Id: *id})
	dump(resp, err)
}

func runList(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	mch := fs.String("mch", "", "mch_id")
	page := fs.Int("page", 1, "page")
	size := fs.Int("size", 20, "page_size")
	_ = fs.Parse(args)
	cli := orderv1.NewPaymentIntentServiceClient(conn)
	resp, err := cli.List(ctx, &orderv1.ListPaymentIntentsRequest{MchId: *mch, Page: int32(*page), PageSize: int32(*size)})
	dump(resp, err)
}

func runRefund(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("refund", flag.ExitOnError)
	pi := fs.String("pi", "", "payment_intent_id")
	ch := fs.String("charge", "", "charge_id (optional)")
	amt := fs.Int64("amount", 0, "amount (0 = full)")
	reason := fs.String("reason", "", "reason: duplicate / fraudulent / requested_by_customer / expired_uncaptured")
	_ = fs.Parse(args)
	cli := orderv1.NewRefundServiceClient(conn)
	resp, err := cli.Create(ctx, &orderv1.CreateRefundRequest{
		PaymentIntentId: *pi, ChargeId: *ch, Amount: *amt,
		Reason: parseRefundReason(*reason),
	})
	dump(resp, err)
}

func runListCharges(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("list-charges", flag.ExitOnError)
	pi := fs.String("pi", "", "payment_intent_id")
	_ = fs.Parse(args)
	cli := orderv1.NewChargeServiceClient(conn)
	resp, err := cli.List(ctx, &orderv1.ListChargesRequest{PaymentIntentId: *pi})
	dump(resp, err)
}

func runListRefunds(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("list-refunds", flag.ExitOnError)
	pi := fs.String("pi", "", "payment_intent_id")
	_ = fs.Parse(args)
	cli := orderv1.NewRefundServiceClient(conn)
	resp, err := cli.List(ctx, &orderv1.ListRefundsRequest{PaymentIntentId: *pi})
	dump(resp, err)
}

func runWebhook(ctx context.Context, conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("webhook", flag.ExitOnError)
	channelName := fs.String("channel", "payment-core", "channel_name registered in PaymentChannelRegistry")
	dir := fs.String("dir", "channel", "direction: channel | client")
	eventID := fs.String("event_id", "", "event_id (idempotency)")
	eventType := fs.String("event", "", "event_type, e.g. charge.succeeded")
	pi := fs.String("pi", "", "pi_id")
	charge := fs.String("charge", "", "charge_id")
	refund := fs.String("refund", "", "refund_id")
	extRef := fs.String("ext", "", "external_ref_no")
	_ = fs.Parse(args)
	body := fmt.Sprintf("event_id=%s&event_type=%s&pi_id=%s&charge_id=%s&refund_id=%s&external_ref_no=%s",
		*eventID, *eventType, *pi, *charge, *refund, *extRef)
	d := orderv1.WebhookDirection_WEBHOOK_DIRECTION_CHANNEL
	if *dir == "client" {
		d = orderv1.WebhookDirection_WEBHOOK_DIRECTION_CLIENT
	}
	cli := orderv1.NewWebhookServiceClient(conn)
	resp, err := cli.Ingest(ctx, &orderv1.IngestWebhookRequest{
		Direction:   d,
		ChannelName: *channelName,
		Body:        []byte(body),
	})
	dump(resp, err)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func parseRefundReason(s string) orderv1.RefundReason {
	switch s {
	case "duplicate":
		return orderv1.RefundReason_REFUND_REASON_DUPLICATE
	case "fraudulent":
		return orderv1.RefundReason_REFUND_REASON_FRAUDULENT
	case "requested_by_customer":
		return orderv1.RefundReason_REFUND_REASON_REQUESTED_BY_CUSTOMER
	case "expired_uncaptured":
		return orderv1.RefundReason_REFUND_REASON_EXPIRED_UNCAPTURED
	}
	return orderv1.RefundReason_REFUND_REASON_UNSPECIFIED
}

func csv(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func dump(v any, err error) {
	if err != nil {
		die(err)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
