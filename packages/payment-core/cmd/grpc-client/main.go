// Command grpc-client (Kitex 版) — payment-core 调试工具.
//
//	go run ./cmd/grpc-client -addr 127.0.0.1:9090 charge \
//	  -pi pi_4371234560001 -amount 10000 -country PH -pm GCASH
//	go run ./cmd/grpc-client refund -pi pi_x -ref rfn_y -amount 500 -pm GCASH -country PH
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/xiongwp/payment-util/kitexutil"

	paymentcorev1 "reconcile-system/packages/payment-core/kitex_gen/paymentcore/v1"
	paymentcoreservice "reconcile-system/packages/payment-core/kitex_gen/paymentcore/v1/paymentcoreservice"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9090", "payment-core kitex address")
	shadowFl := flag.Bool("shadow", false, "send as shadow traffic (x-shadow=1; 透传给 channel/risk/kms)")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: grpc-client [-addr host:port] <charge|refund|query|capture|void> [opts]")
		os.Exit(2)
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	pi := fs.String("pi", "pi_4471234560001", "payment_intent id")
	charge := fs.String("charge", "", "charge id (required for capture/void/refund)")
	refund := fs.String("refund", "", "refund id (required for refund)")
	amount := fs.Int64("amount", 10000, "amount in minor units")
	currency := fs.String("currency", "PHP", "currency")
	country := fs.String("country", "PH", "country ISO2")
	pm := fs.String("pm", "GCASH", "payment_method (GCASH/MAYA/GRABPAY/...)")
	adapter := fs.String("adapter", "", "explicit adapter name (optional, overrides routing)")
	ref := fs.String("ref", "", "external_ref_no (refund/query/capture/void)")
	reason := fs.String("reason", "requested_by_customer", "refund reason")
	returnURL := fs.String("return-url", "https://cashier.example/ret", "return url")
	_ = fs.Parse(args[1:])

	cli, err := paymentcoreservice.NewClient("payment-core",
		client.WithHostPorts(*addr),
		client.WithRPCTimeout(30*time.Second),
	)
	if err != nil {
		die(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if *shadowFl {
		// shadow header 透传: 等 kitexutil.ShadowMW 接好后这里换成 kitexutil.WithShadow(ctx).
		// 当前 stub: 用 admin token slot 做占位 (kitexutil 自定义 ctx key 通道).
		_ = kitexutil.WithAdminToken
		fmt.Fprintln(os.Stderr, "[grpc-client] shadow=1 — TODO: wire kitexutil.WithShadow")
	}

	extra := map[string]string{"country": *country, "payment_method": *pm}
	if *adapter != "" {
		extra["adapter"] = *adapter
	}

	switch cmd {
	case "charge":
		resp, err := cli.Charge(ctx, &paymentcorev1.ChargeRequest{
			PaymentIntentId: *pi,
			Amount:          *amount,
			Currency:        *currency,
			Country:         *country,
			PaymentMethod:   *pm,
			ReturnUrl:       *returnURL,
			Description:     "grpc-client charge",
		})
		if err != nil {
			die(err)
		}
		fmt.Printf("charge ok: result=%s external_ref=%s\n", resp.GetResultType(), resp.GetExternalRefNo())
		if ra := resp.GetRequiredAction(); ra != nil {
			fmt.Printf("  required_action: type=%s details=%v\n", ra.GetType(), ra.GetDetails())
		}
	case "capture":
		resp, err := cli.Capture(ctx, &paymentcorev1.CaptureRequest{
			PaymentIntentId: *pi,
			ChargeId:        *charge,
			ExternalRefNo:   *ref,
			Amount:          *amount,
			Extra:           extra,
		})
		if err != nil {
			die(err)
		}
		fmt.Printf("capture ok: result=%s\n", resp.GetResultType())
	case "void":
		resp, err := cli.Void(ctx, &paymentcorev1.VoidRequest{
			PaymentIntentId: *pi,
			ChargeId:        *charge,
			ExternalRefNo:   *ref,
			Reason:          *reason,
			Extra:           extra,
		})
		if err != nil {
			die(err)
		}
		fmt.Printf("void ok: result=%s\n", resp.GetResultType())
	case "refund":
		resp, err := cli.Refund(ctx, &paymentcorev1.RefundRequest{
			PaymentIntentId: *pi,
			ChargeId:        *charge,
			RefundId:        *refund,
			ExternalRefNo:   *ref,
			Amount:          *amount,
			Currency:        *currency,
			Reason:          *reason,
			Extra:           extra,
		})
		if err != nil {
			die(err)
		}
		fmt.Printf("refund ok: result=%s external_ref=%s\n", resp.GetResultType(), resp.GetExternalRefNo())
	case "query":
		resp, err := cli.Query(ctx, &paymentcorev1.QueryRequest{
			PaymentIntentId: *pi,
			ChargeId:        *charge,
			ExternalRefNo:   *ref,
			Extra:           extra,
		})
		if err != nil {
			die(err)
		}
		fmt.Printf("query ok: result=%s captured=%d refunded=%d\n",
			resp.GetResultType(), resp.GetAmountCaptured(), resp.GetAmountRefunded())
	default:
		die(fmt.Errorf("unknown command %q", cmd))
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
