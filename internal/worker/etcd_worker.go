package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func Register(cli *clientv3.Client) int64 {
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("/snowflake/%d", i)
		log.Println("key:", key)
		lease, err := cli.Grant(context.Background(), 30)
		if err != nil {
			log.Println("grant failed:", err)
			continue
		} else {
			log.Printf("grant lease %d successfully\n", lease.ID)
		}

		resp, err := cli.Txn(context.Background()).
			If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
			Then(clientv3.OpPut(key, "1", clientv3.WithLease(lease.ID))).
			Commit()

		if err != nil {
			log.Println("txn failed:", err)
			continue
		}

		if resp.Succeeded {
			go cli.KeepAlive(context.Background(), lease.ID)
			os.WriteFile("/tmp/worker", []byte(fmt.Sprint(i)), 0644)
			return int64(i)
		} else {
			log.Printf("worker %d already exists\n", i)
		}
	}

	// fallback
	data, _ := os.ReadFile("/tmp/worker")
	id, _ := strconv.ParseInt(string(data), 10, 64)
	return id
}
