// id-generator server — uber/fx + Kitex (Protobuf IDL).
//
// 跟 monorepo 内 30 个服务 uber/fx 风格一致; RPC 层用 CloudWeGo Kitex 替换 google.golang.org/grpc.
//
// 部署:
//
//	./id-generator
//	→ Kitex listen :9090 (TTHeader+Protobuf), 自注册到 etcd /recon/services/id-generator/
//	→ 调用方走 kitexutil.EtcdResolver 拿到副本列表 round_robin
//
// 生成 kitex_gen:
//
//	./idl/generate.sh idgen
//	(必须先跑一遍, 否则下面 import 编译不过 — kitex_gen/idgen/v1/idgenservice 是生成产物)
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/server"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"github.com/xiongwp/id-generator/internal/generator"
	idgenpb "github.com/xiongwp/id-generator/kitex_gen/idgen/v1"
	idgenservice "github.com/xiongwp/id-generator/kitex_gen/idgen/v1/idgenservice"
	"github.com/xiongwp/id-generator/internal/segment"
	"github.com/xiongwp/id-generator/internal/service"
	"github.com/xiongwp/id-generator/internal/worker"
	"github.com/xiongwp/payment-util/kitexutil"

	_ "github.com/go-sql-driver/mysql"
)

// workerID / regionID 单独类型 — fx 注入识别.
type workerID int64
type regionID int64

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newEtcdClient,
			newWorkerID,
			newRegionID,
			newSnowflake,
			newDB,
			newMainBuffer,
			newShadowBuffer,
			newIDServiceImpl,
			newKitexServer,
		),
		fx.Invoke(startKitexServer),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
	).Run()
}

func newLogger(lc fx.Lifecycle) (*zap.Logger, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { _ = logger.Sync(); return nil }})
	return logger, nil
}

func newEtcdClient(lc fx.Lifecycle, log *zap.Logger) *clientv3.Client {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"accounting-etcd:2379"},
		DialTimeout: 60 * time.Second,
	})
	if err != nil {
		log.Warn("etcd connect failed, fallback to local", zap.Error(err))
		return nil
	}
	log.Info("etcd client connected", zap.Strings("endpoints", []string{"accounting-etcd:2379"}))
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			if err := cli.Close(); err != nil {
				log.Warn("etcd close error", zap.Error(err))
				return err
			}
			return nil
		},
	})
	return cli
}

func newWorkerID(cli *clientv3.Client, log *zap.Logger) workerID {
	wid := worker.Register(cli)
	log.Info("workerID registered", zap.Int64("workerID", wid))
	return workerID(wid)
}

func newRegionID() regionID { return regionID(1) }

func newSnowflake(rid regionID, wid workerID) *generator.Snowflake {
	return generator.New(int64(rid), int64(wid))
}

func newDB(log *zap.Logger) (*gorm.DB, error) {
	dsn := "root:password@tcp(mysql-generator:3306)/idgen?charset=utf8mb4&parseTime=True&loc=Local&tls=false"
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Error("gorm open failed", zap.Error(err))
		return nil, fmt.Errorf("gorm.Open: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("sqlDB: %w", err)
	}
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetMaxIdleConns(20)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	if err := sqlDB.Ping(); err != nil {
		log.Error("mysql ping failed", zap.Error(err))
		return nil, fmt.Errorf("ping: %w", err)
	}
	log.Info("id-generator DB connected")
	return db, nil
}

func newMainBuffer(db *gorm.DB, log *zap.Logger) *segment.MainBuffer {
	buf := segment.NewMainBuffer(db)
	buf.Load()
	log.Info("main segment buffer loaded")
	return buf
}

func newShadowBuffer(db *gorm.DB, log *zap.Logger) *segment.ShadowBuffer {
	buf := segment.NewShadowBuffer(db)
	buf.Load()
	log.Info("shadow segment buffer loaded")
	return buf
}

// newIDServiceImpl 把 service.Server (内部 handler) 包装成 Kitex IDService 接口.
//
// service.Server 原本满足 grpc pb.IDServiceServer; 用 Kitex 时签名一样 (protobuf 同一份 IDL),
// 走 kitex_gen/idgen/v1/idgenservice 的接口定义即可.
func newIDServiceImpl(sf *generator.Snowflake, main *segment.MainBuffer, sh *segment.ShadowBuffer) idgenpb.IDServiceServer {
	return &service.Server{
		Sf:        sf,
		SegMain:   main,
		SegShadow: sh,
	}
}

// newKitexServer 构造 Kitex server + middleware 链.
//
// 跟 grpc.NewServer + grpc.ChainUnaryInterceptor 等价 — 这里用 kitexutil 共享 MW:
//   - RecoverMW: panic recover
//   - LogMW: access log
//   - MetricsMW: count + latency
//
// 注: 老 shadow.UnaryServerInterceptor 是 gRPC interceptor, 切 Kitex 时需要重写一份
// shadow.KitexMW (从 metainfo 取 shadow 标志位写 ctx). 当前留 TODO.
func newKitexServer(impl idgenpb.IDServiceServer, log *zap.Logger) server.Server {
	const port = 9090
	addr, _ := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
	advHost := os.Getenv("ADVERTISE_HOST")
	if advHost == "" {
		advHost = "id-service"
	}
	srvOpts := []server.Option{
		server.WithServiceAddr(addr),
		server.WithSuite(rpcInfoSuite{}),
	}
	srvOpts = append(srvOpts, kitexutil.DefaultServerOptions("id-generator", fmt.Sprintf("%s:%d", advHost, port))...)
	// kitexutil 共享 MW (3 条标准链):
	// TODO: shadow MW (替换老 shadow.UnaryServerInterceptor) — 等 Kitex shadow port 完成
	srv := idgenservice.NewServer(impl, srvOpts...)
	_ = log
	return srv
}

// startKitexServer lifecycle: OnStart 后台 Run; OnStop graceful Stop.
func startKitexServer(lc fx.Lifecycle, srv server.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("id-generator Kitex listening", zap.String("addr", ":9090"))
			go func() {
				if err := srv.Run(); err != nil {
					log.Error("Kitex serve failed", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error {
			if err := srv.Stop(); err != nil {
				log.Warn("Kitex stop error", zap.Error(err))
			}
			log.Info("id-generator Kitex stopped")
			return nil
		},
	})
	// 防 import "kitexutil" 未引用 — 真实接 MW 时去掉这行 (newKitexServer 内部用).
	_ = kitexutil.LogMW
}

// rpcInfoSuite — 把 RPC 元信息 (service / method / caller) 暴露给 Kitex middleware.
//
// 占位 stub; 实际接 Kitex 时 server.WithSuite 接 kitex/server/genericserver / nphttp2 etc.
// 这里只满足 server.Suite 接口形态, 让代码先编过.
type rpcInfoSuite struct{}

func (rpcInfoSuite) Options() []server.Option { return nil }

var _ rpcinfo.RPCInfo = (*rpcinfo.RPCInfo)(nil) // 防 rpcinfo 导入未用
