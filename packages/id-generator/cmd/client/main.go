// id-generator CLI (Kitex 版).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/transmeta"

	pb "reconcile-system/packages/id-generator/kitex_gen/idgen/v1"
	"reconcile-system/packages/id-generator/kitex_gen/idgen/v1/idservice"
)

var (
	addr     = flag.String("addr", "localhost:9090", "Kitex server address (host:port)")
	timeout  = flag.Duration("timeout", 10*time.Second, "per-call timeout")
	shadowFl = flag.Bool("shadow", false, "send as shadow traffic (x-shadow=1; routes to id_segment_shadow)")
)

func main() {
	flag.Parse()

	_ = timeout // 用 ctx 控制单次 RPC 超时, 这里只声明.

	cli, err := idservice.NewClient("id-generator",
		client.WithHostPorts(*addr),
		client.WithTransportProtocol(transmeta.TTHeader),
	)
	if err != nil {
		log.Fatalf("dial %s failed: %v", *addr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if *shadowFl {
		// TTHeader 透传 — 在 server 侧用 metainfo.GetValue(ctx, "x-shadow") 读.
		ctx = rpcinfo.PutMetaInfoInContext(ctx, "x-shadow", "1")
		fmt.Println("🌑 shadow=1 — server will route to id_segment_shadow")
	}

	resp, err := cli.GetID(ctx, &pb.IDRequest{UserId: 12345})
	if err != nil {
		log.Fatal("❌ rpc failed:", err)
	}
	fmt.Println("🎯 ID:", resp.Id)
}
