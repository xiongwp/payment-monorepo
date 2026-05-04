// Package serviceregistry centralizes etcd-based service discovery, gRPC
// load-balanced dialing, and leader election for the payment platform.
//
// 三件套：
//
//	Registrar   服务端启动时把自己的 (service, addr) 注册到 etcd，TTL lease + 心跳；
//	            进程退出 / panic / OOM 后 lease 失效，etcd 自动剔除该端点。
//	Resolver    自定义 gRPC resolver 把 "etcd:///<service>" 解析成 etcd 里所有
//	            活节点；新副本上线 / 旧副本下线 → grpc-go ClientConn 实时刷新；
//	            内置 round_robin LB 均摊请求。
//	Election    cron / worker 单例化：所有副本竞争同一个 etcd key，只有 leader
//	            真正跑任务。Leader 挂掉后 lease 过期，下一副本秒级接管。
//
// 典型用法（server）：
//
//	cli, _ := serviceregistry.NewEtcdClient([]string{"etcd:2379"})
//	defer cli.Close()
//	reg, _ := serviceregistry.NewRegistrar(cli, "accounting-service", "10.0.0.5:50051")
//	_ = reg.Register(ctx, 10*time.Second)
//	defer reg.Close()
//
// 典型用法（client）：
//
//	cli, _ := serviceregistry.NewEtcdClient([]string{"etcd:2379"})
//	_ = serviceregistry.RegisterResolver(cli)
//	conn, _ := serviceregistry.Dial("accounting-service",
//	    grpc.WithTransportCredentials(insecure.NewCredentials()))
//
// 典型用法（leader election worker）：
//
//	el, _ := serviceregistry.NewElection(cli, "/leader/order-core/expire-worker", 10*time.Second)
//	defer el.Close()
//	for {
//	    if err := el.Campaign(ctx, hostID); err != nil { return }
//	    runCronUntil(el.Done())   // session 过期就退出循环重新竞选
//	}
package serviceregistry
