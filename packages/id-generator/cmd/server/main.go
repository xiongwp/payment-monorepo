package main

import (
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"log"
	"net"

	"github.com/xiongwp/id-generator/internal/generator"
	"github.com/xiongwp/id-generator/internal/segment"
	"github.com/xiongwp/id-generator/internal/service"
	"github.com/xiongwp/id-generator/internal/worker"
	"github.com/xiongwp/payment-util/shadow"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	pb "github.com/xiongwp/id-generator/internal/proto"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	// ================================
	// 1️⃣ 初始化 metrics
	// ================================
	//metrics.Init()

	go func() {
		//http.Handle("/metrics", promhttp.Handler())
		log.Println("metrics server at :2112")
		//log.Fatal(http.ListenAndServe(":2112", nil))
	}()

	// ================================
	// 2️⃣ 初始化 etcd（workerId）
	// ================================
	log.Println("start server")
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"accounting-etcd:2379"},
		DialTimeout: 60 * time.Second,
	})
	if err != nil {
		log.Println("etcd connect failed, fallback to local:", err)
	} else {
		log.Println("start etcd client successfully")
	}

	workerID := worker.Register(cli)
	log.Println("workerID:", workerID)

	// ================================
	// 3️⃣ 初始化 Snowflake++
	// ================================
	regionID := int64(1) // 你可以配置化
	sf := generator.New(regionID, workerID)

	// ================================
	// 4️⃣ 初始化 DB（segment）
	// ================================
	db, err := NewDB()
	if err != nil {
		log.Fatal("failed to connect to database:", err)
	} else {
		log.Println("success started to connect database")
	}

	// ================================
	// 5️⃣ 初始化 segment buffer（主 + 影子各一个）
	// ================================
	bufMain := segment.NewMainBuffer(db)
	bufMain.Load() // 预加载主号段

	// 影子 buffer 用同一个 db connection（id_segment / id_segment_shadow 在同库）。
	// init_shadow.sql 必须已经导入；否则 Load 会 fatal — 这是有意为之，让运维
	// 看到 shadow 表缺失第一时间发现。
	bufShadow := segment.NewShadowBuffer(db)
	bufShadow.Load() // 预加载影子号段

	// ================================
	// 6️⃣ 启动 gRPC 服务（装 shadow interceptor 翻 metadata 进 ctx）
	// ================================
	lis, err := net.Listen("tcp", ":9090")
	if err != nil {
		log.Fatal(err)
	}

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			shadow.UnaryServerInterceptor(),
		),
	)

	pb.RegisterIDServiceServer(grpcServer, &service.Server{
		Sf:        sf,
		SegMain:   bufMain,
		SegShadow: bufShadow,
	})

	log.Println("gRPC server started at :9090")

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatal(err)
	}
}
func NewDB() (*gorm.DB, error) {

	dsn := "root:password@tcp(mysql-generator:3306)/idgen?charset=utf8mb4&parseTime=True&loc=Local&tls=false"

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Println("failed to connect database:", err)
		return nil, err
	}

	// 🔥 获取底层 sql.DB（连接池在这里配）
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}

	// ✅ 连接池（关键）
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetMaxIdleConns(20)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)

	// ✅ 必须 Ping（和你原来一样）
	if err := sqlDB.Ping(); err != nil {
		return nil, err
	}

	return db, nil
}
