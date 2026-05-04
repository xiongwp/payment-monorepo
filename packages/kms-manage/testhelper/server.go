// Package testhelper 给其它仓库做端到端测试时"启动一个真实的 kms-manage
// gRPC 服务器"用。内部 keystore 直接从一张 map[keyID]hexKey 构造，
// 不落盘，测试毫秒级启停。
//
// 本包路径在 repo 根外（非 internal/），跨模块可以 import。
package testhelper

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	"github.com/xiongwp/kms-manage/internal/keystore"
	"github.com/xiongwp/kms-manage/internal/metrics"
	"github.com/xiongwp/kms-manage/internal/server"
	"github.com/xiongwp/kms-manage/internal/service"
)

func init() { metrics.Register() }

// StartConfig 指定 keystore 内容。HexKeys 是 key_id → hex-string 的映射，Active 指向其中一个。
type StartConfig struct {
	HexKeys map[string]string
	Active  string
}

// DefaultConfig 返回一个用于绝大多数测试的 "main" 32-byte key 配置。
func DefaultConfig() StartConfig {
	return StartConfig{
		HexKeys: map[string]string{
			"main": "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
		},
		Active: "main",
	}
}

// Server 持有启动的 kms-manage 实例 + 拨号 client。
type Server struct {
	Client   kmsv1.KMSServiceClient
	Conn     *grpc.ClientConn
	Listener *bufconn.Listener
	cleanup  func()
}

func (s *Server) Stop() { s.cleanup() }

// Start 起一个 bufconn 上的真实 kms-manage server；测试结束自动 cleanup。
// t 接受 *testing.T / *testing.B。
func Start(t testing.TB, cfg StartConfig) *Server {
	t.Helper()

	if len(cfg.HexKeys) == 0 {
		cfg = DefaultConfig()
	}
	if cfg.Active == "" {
		for k := range cfg.HexKeys {
			cfg.Active = k
			break
		}
	}

	dir := t.TempDir()
	for id, h := range cfg.HexKeys {
		if _, err := hex.DecodeString(strings.Join(strings.Fields(h), "")); err != nil {
			t.Fatalf("testhelper: bad hex for %s: %v", id, err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".key"), []byte(h), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ACTIVE"), []byte(cfg.Active), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := keystore.Load(dir)
	if err != nil {
		t.Fatalf("testhelper: load keystore: %v", err)
	}
	logger := zap.NewNop()
	svc := service.NewKMSService(store, logger)
	handler := server.NewServer(server.Deps{KMSSvc: svc, Logger: logger})

	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	kmsv1.RegisterKMSServiceServer(grpcSrv, handler)
	go func() {
		if err := grpcSrv.Serve(lis); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Logf("kms testhelper serve: %v", err)
		}
	}()

	conn, err := grpc.NewClient("passthrough://kms",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
		_ = lis.Close()
	}
	t.Cleanup(cleanup)
	return &Server{
		Client:   kmsv1.NewKMSServiceClient(conn),
		Conn:     conn,
		Listener: lis,
		cleanup:  cleanup,
	}
}

// EncryptForTest 测试辅助：用 server 加密一段明文，返回线格式密文。
func EncryptForTest(t *testing.T, s *Server, context, plaintext string) string {
	t.Helper()
	resp, err := s.Client.Encrypt(contextBG(), &kmsv1.EncryptRequest{
		Plaintext: []byte(plaintext),
		Context:   context,
	})
	if err != nil {
		t.Fatalf("encrypt for test: %v", err)
	}
	return resp.GetCiphertext()
}

func contextBG() context.Context { return context.Background() }

// Ensure we don't leak fmt import if compiler trims later additions.
var _ = fmt.Sprint
