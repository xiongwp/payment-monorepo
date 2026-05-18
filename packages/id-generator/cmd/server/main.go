// id-generator server 入口 — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
//
// gRPC: :9090 提供 IDService (snowflake + segment 双源).
// etcd 注册 workerId, MySQL 存 segment range.
package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/xiongwp/id-generator/internal/generator"
	pb "github.com/xiongwp/id-generator/internal/proto"
	"github.com/xiongwp/id-generator/internal/segment"
	"github.com/xiongwp/id-generator/internal/service"
	"github.com/xiongwp/id-generator/internal/worker"
	"github.com/xiongwp/payment-util/shadow"

	_ "github.com/go-sql-driver/mysql"
)

// workerID 单独类型让 fx 识别 Provider, 跟其它 int64 区分.
type workerID int64

// regionID 同上 — 区分 fx 注入.
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
			newIDService,
			newGRPCServer,
		),
		fx.Invoke(startGRPCServer),
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

// newRegionID 当前固定 1, 后续可换 env / config-center.
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

func newIDService(sf *generator.Snowflake, main *segment.MainBuffer, sh *segment.ShadowBuffer) *service.Server {
	return &service.Server{
		Sf:        sf,
		SegMain:   main,
		SegShadow: sh,
	}
}

func newGRPCServer(svc *service.Server) *grpc.Server {
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			shadow.UnaryServerInterceptor(),
		),
	)
	pb.RegisterIDServiceServer(srv, svc)
	return srv
}

func startGRPCServer(lc fx.Lifecycle, srv *grpc.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			lis, err := net.Listen("tcp", ":9090")
			if err != nil {
				log.Error("listen failed", zap.Error(err))
				return err
			}
			log.Info("id-generator gRPC listening", zap.String("addr", ":9090"))
			go func() {
				if err := srv.Serve(lis); err != nil {
					log.Error("gRPC serve failed", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error {
			srv.GracefulStop()
			log.Info("id-generator gRPC stopped")
			return nil
		},
	})
}
