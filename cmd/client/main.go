package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	pb "github.com/xiongwp/id-generator/internal/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	addr    = flag.String("addr", "localhost:9090", "gRPC server address")
	timeout = flag.Duration("timeout", 10*time.Second, "per-call timeout")
)

func main() {

	flag.Parse()

	conn, err := grpc.Dial(*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		log.Fatalf("dial %s failed: %v", *addr, err)
	}
	defer conn.Close()

	if err != nil {
		log.Fatal("❌ connect failed:", err)
	}
	defer conn.Close()

	client := pb.NewIDServiceClient(conn)

	// 超时控制
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	userID := int64(12345)

	resp, err := client.GetID(ctx, &pb.IDRequest{
		UserId: userID,
	})
	if err != nil {
		log.Fatal("❌ rpc failed:", err)
	}

	id := resp.Id

	fmt.Println("🎯 ID:", id)
}
