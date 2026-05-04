package serviceregistry

import (
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// NewEtcdClient creates an etcd v3 client with defaults tuned for service registry
// use (5s dial timeout, no auto-sync). Caller is responsible for Close.
func NewEtcdClient(endpoints []string) (*clientv3.Client, error) {
	return clientv3.New(clientv3.Config{
		Endpoints:            endpoints,
		DialTimeout:          5 * time.Second,
		DialKeepAliveTime:    30 * time.Second,
		DialKeepAliveTimeout: 5 * time.Second,
	})
}
