// Command grpc-client (Kitex 版) — payment-channel 调试工具.
//
//	go run ./cmd/grpc-client -addr 127.0.0.1:9092 charge -adapter gcash -pi pi_x -amount 10000
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cloudwego/kitex/client"

	channelv1 "github.com/xiongwp/payment-channel/kitex_gen/channel/v1"
	acquirerservice "github.com/xiongwp/payment-channel/kitex_gen/channel/v1/acquirerservice"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9092", "payment-channel kitex address")
	shadowFl := flag.Bool("shadow", false, "shadow=1 — adapter 入口短路返回 mock, 不真发外部渠道")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: grpc-client [flags] <charge|refund|query> [opts]")
		os.Exit(2)
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	adapter := fs.String("adapter", "gcash", "adapter name")
	pi := fs.String("pi", "pi_4371234560001", "pi id")
	idem := fs.String("idem", "", "idempotency key (default sha of pi+action)")
	amount := fs.Int64("amount", 10000, "amount in minor units")
	currency := fs.String("currency", "PHP", "currency")
	ref := fs.String("ref", "", "external_ref_no (refund/query)")
	reason := fs.String("reason", "requested_by_customer", "refund reason")
	_ = fs.Parse(args[1:])

	cli, err := acquirerservice.NewClient("payment-channel",
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
		fmt.Fprintln(os.Stderr, "[grpc-client] shadow=1 — TODO: wire kitexutil.WithShadow")
	}

	idk := *idem
	if idk == "" {
		idk = fmt.Sprintf("%s:%s", *pi, cmd)
	}

	switch cmd {
	case "charge":
		resp, err := cli.Charge(ctx, &channelv1.ChargeRequest{
			Adapter:        *adapter,
			PiId:           *pi,
			IdempotencyKey: idk,
			Amount:         *amount,
			Currency:       *currency,
		})
		if err != nil {
			die(err)
		}
		fmt.Printf("charge ok: result=%s external_ref=%s\n", resp.GetResult(), resp.GetExternalRefNo())
	case "refund":
		resp, err := cli.Refund(ctx, &channelv1.RefundRequest{
			Adapter:        *adapter,
			PiId:           *pi,
			ExternalRefNo:  *ref,
			Amount:         *amount,
			Reason:         *reason,
			IdempotencyKey: idk,
		})
		if err != nil {
			die(err)
		}
		fmt.Printf("refund ok: result=%s external_ref=%s\n", resp.GetResult(), resp.GetExternalRefNo())
	case "query":
		resp, err := cli.Query(ctx, &channelv1.QueryRequest{
			Adapter:       *adapter,
			PiId:          *pi,
			ExternalRefNo: *ref,
		})
		if err != nil {
			die(err)
		}
		fmt.Printf("query ok: result=%s captured=%d refunded=%d\n",
			resp.GetResult(), resp.GetAmountCaptured(), resp.GetAmountRefunded())
	default:
		die(fmt.Errorf("unknown command %q", cmd))
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
