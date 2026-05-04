// Command mockserver runs the in-process PH payment channel mock as a
// standalone HTTP server. Use it during local development and CI e2e tests
// when you don't want to hit real sandbox endpoints.
//
// Example:
//
//	go run ./cmd/mockserver -addr :9400 -webhook-delay 200ms
//
// Point any adapter at it via its Config.BaseURL (or PAYCHAN_CHANNEL_*_BASE_URL
// env var). The mock prints a GCash public key PEM on startup — copy it into
// channel.gcash.gcash_pub_key so the adapter trusts webhook signatures.
package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/xiongwp/payment-channel/internal/mockserver"
)

func main() {
	addr := flag.String("addr", ":9400", "listen address")
	webhookDelay := flag.Duration("webhook-delay", 200*time.Millisecond, "delay before async webhook fires")
	printKey := flag.Bool("print-gcash-pubkey", true, "print the mock GCash gateway public key on startup")
	flag.Parse()

	srv, err := mockserver.New(mockserver.Options{
		Logger:       mockserver.NewStdLogger(),
		WebhookDelay: *webhookDelay,
	})
	if err != nil {
		log.Fatalf("mockserver: %v", err)
	}

	if *printKey {
		fmt.Println("# GCash mock gateway public key — put into channel.gcash.gcash_pub_key")
		fmt.Println(srv.GCashPublicKeyPEM())
	}
	if err := srv.ListenAndServe(*addr); err != nil {
		log.Fatalf("mockserver: %v", err)
	}
}
