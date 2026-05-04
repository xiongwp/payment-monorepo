// Package testhelper 提供给其它仓库做端到端测试的"启动一个真实的
// payment-channel gRPC 服务器"工具。仓储走内存替身，adapter 由调用方注入
// scripted 实现，外部渠道的 HTTP 请求全部不会发出。
//
// 本包路径在 repo 根外，跨模块可以 import（不受 internal 访问限制）。
package testhelper

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	channelv1 "github.com/xiongwp/payment-channel/api/proto/channel/v1"
	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/server"
	"github.com/xiongwp/payment-channel/internal/service"
)

// ScriptedChargeResult 描述 scripted adapter 的 Charge 返回值。
type ScriptedChargeResult struct {
	Result         string // succeeded / requires_action / processing / authorized / failed
	ExternalRefNo  string
	RedirectURL    string // 非空时自动产 RequiredAction{app_redirect}
	ReturnURL      string
	ExpiresAt      time.Time
	FailureCode    string
	FailureMessage string
	// RequiredAction 完全自定义动作（替代 RedirectURL 路径）。
	// 用法：测试 mini-program / OTP / 3DS 等非 app_redirect 的 action 类型时，
	// 直接构造一个 *ScriptedRequiredAction 注入。设置后 RedirectURL 字段被忽略。
	RequiredAction *ScriptedRequiredAction
}

// ScriptedRequiredAction 跨模块可见的 RequiredAction 镜像。internal/channel 不能
// 跨 module import，所以这里提供一个公开 mirror。字段语义与 channel.RequiredAction 一致。
type ScriptedRequiredAction struct {
	Type        string
	ExpiresAt   time.Time
	RedirectURL string
	Scheme      string
	ReturnURL   string
	QRCodeURL   string
	QRImageB64  string
	PollMs      int
	OTPMasked   string
	OTPChannel  string
	OTPLength   int
	Extra       map[string]string
}

// CapturedCharge 是给跨模块测试用的 Charge 请求快照。
// 故意不暴露内部 channel.ChargeRequest——它在 internal 包里，跨模块不可 import。
// 只镜像测试用得到的字段；按需添加新字段不会破坏现有调用方。
type CapturedCharge struct {
	PiID             string
	Adapter          string
	IdempotencyKey   string
	Amount           int64
	Currency         string
	Description      string
	ReturnURL        string
	CaptureImmediate bool
	Metadata         map[string]string
}

// ScriptedAdapter 用来配置 mock 外部渠道。
type ScriptedAdapter struct {
	Name             string
	Charge           ScriptedChargeResult     // Charge 的 canned 返回
	OverrideCharge   func(pi string) ScriptedChargeResult // 可选：按 pi 动态返回
	// OnCharge 收到 Charge 请求时调用（在 canned 响应返回前）。
	// 测试用来 assert metadata / amount / pi_id 等是否被正确透传。
	// 此 callback 仅作 side-effect 用途，不影响响应。
	OnCharge func(c CapturedCharge)
}

// Server 持有启动的 payment-channel 实例 + 拨号 client。
type Server struct {
	Client  channelv1.AcquirerServiceClient
	Conn    *grpc.ClientConn
	Listener *bufconn.Listener
	cleanup func()
}

// Stop 优雅停止 + 释放资源。
func (s *Server) Stop() { s.cleanup() }

// Start 启动一个真实的 payment-channel AcquirerService gRPC server（在 bufconn 上），
// 用内存仓储 + 传入的 scripted adapter。
//
// t 接受 testing.TB（不是 *testing.T）以便同时支持 Test* 和 Benchmark*；与
// payment-core/testhelper.Start 保持一致。
func Start(t testing.TB, adapters ...ScriptedAdapter) *Server {
	t.Helper()
	logger := zap.NewNop()

	reg := channel.NewRegistry()
	for _, sa := range adapters {
		reg.Register(toInternalAdapter(sa))
	}
	svc := service.NewAcquirerService(reg, newMemTxRepo(), &memIDIssuer{}, logger)
	handler := server.NewServer(server.Deps{AcquirerSvc: svc, AllowUnauthenticated: true, Logger: logger})

	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	channelv1.RegisterAcquirerServiceServer(grpcSrv, handler)
	go func() {
		if err := grpcSrv.Serve(lis); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Logf("paychan testhelper serve: %v", err)
		}
	}()

	conn, err := grpc.NewClient("passthrough://paychan",
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
		Client:   channelv1.NewAcquirerServiceClient(conn),
		Conn:     conn,
		Listener: lis,
		cleanup:  cleanup,
	}
}

// ─── 内部：scripted → channel.Adapter ─────────────────────────

func toInternalAdapter(sa ScriptedAdapter) channel.Adapter {
	return &scriptedAdapterImpl{cfg: sa}
}

type scriptedAdapterImpl struct{ cfg ScriptedAdapter }

func (a *scriptedAdapterImpl) Name() string { return a.cfg.Name }
func (a *scriptedAdapterImpl) Charge(_ context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	if a.cfg.OnCharge != nil {
		// 拷贝 metadata map：避免回调持有指向后续请求的 map（跨测试污染）
		var md map[string]string
		if req.Metadata != nil {
			md = make(map[string]string, len(req.Metadata))
			for k, v := range req.Metadata {
				md[k] = v
			}
		}
		a.cfg.OnCharge(CapturedCharge{
			PiID:             req.PiID,
			Adapter:          a.cfg.Name,
			IdempotencyKey:   req.IdempotencyKey,
			Amount:           req.Amount,
			Currency:         req.Currency,
			Description:      req.Description,
			ReturnURL:        req.ReturnURL,
			CaptureImmediate: req.CaptureImmediate,
			Metadata:         md,
		})
	}
	r := a.cfg.Charge
	if a.cfg.OverrideCharge != nil {
		r = a.cfg.OverrideCharge(req.PiID)
	}
	resp := &channel.ChargeResponse{
		Result:         channel.ResultType(nonEmpty(r.Result, "succeeded")),
		ExternalRefNo:  nonEmpty(r.ExternalRefNo, a.cfg.Name+"_"+req.PiID),
		FailureCode:    r.FailureCode,
		FailureMessage: r.FailureMessage,
	}
	switch {
	case r.RequiredAction != nil:
		// 自定义 action（mini_program_invoke 等非 redirect 类）。
		// 把跨模块 mirror 拷贝成 internal channel.RequiredAction；map 也拷一份
		// 防测试间共享写污染。
		exp := r.RequiredAction.ExpiresAt
		if exp.IsZero() {
			exp = time.Now().Add(15 * time.Minute)
		}
		var extra map[string]string
		if r.RequiredAction.Extra != nil {
			extra = make(map[string]string, len(r.RequiredAction.Extra))
			for k, v := range r.RequiredAction.Extra {
				extra[k] = v
			}
		}
		resp.RequiredAction = &channel.RequiredAction{
			Type:        r.RequiredAction.Type,
			ExpiresAt:   exp,
			RedirectURL: r.RequiredAction.RedirectURL,
			Scheme:      r.RequiredAction.Scheme,
			ReturnURL:   r.RequiredAction.ReturnURL,
			QRCodeURL:   r.RequiredAction.QRCodeURL,
			QRImageB64:  r.RequiredAction.QRImageB64,
			PollMs:      r.RequiredAction.PollMs,
			OTPMasked:   r.RequiredAction.OTPMasked,
			OTPChannel:  r.RequiredAction.OTPChannel,
			OTPLength:   r.RequiredAction.OTPLength,
			Extra:       extra,
		}
	case r.RedirectURL != "":
		exp := r.ExpiresAt
		if exp.IsZero() {
			exp = time.Now().Add(15 * time.Minute)
		}
		resp.RequiredAction = &channel.RequiredAction{
			Type:        "app_redirect",
			RedirectURL: r.RedirectURL,
			ReturnURL:   nonEmpty(r.ReturnURL, req.ReturnURL),
			Scheme:      "universal",
			ExpiresAt:   exp,
		}
	}
	return resp, nil
}

func (a *scriptedAdapterImpl) Capture(_ context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}
func (a *scriptedAdapterImpl) Void(_ context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}
func (a *scriptedAdapterImpl) Refund(_ context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: "rfd_" + req.ExternalRefNo}, nil
}
func (a *scriptedAdapterImpl) Query(_ context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	return &channel.QueryResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}
func (a *scriptedAdapterImpl) ParseWebhook(_ map[string]string, _ []byte) (*channel.WebhookEvent, error) {
	return &channel.WebhookEvent{EventID: "e", PiID: "pi", EventType: "charge.succeeded", SignatureOK: true}, nil
}

func nonEmpty(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

// ─── 内部：内存仓储 ─────────────────────────────────────────

type memTxRepo struct {
	mu   sync.Mutex
	rows []*domain.AcquirerTx
	n    uint64
}

func newMemTxRepo() *memTxRepo { return &memTxRepo{} }

func (r *memTxRepo) Insert(_ context.Context, tx *domain.AcquirerTx) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.Adapter == tx.Adapter && x.IdempotencyKey == tx.IdempotencyKey {
			return domain.ErrIdempotentHit
		}
	}
	r.n++
	tx.ID = r.n
	cp := *tx
	r.rows = append(r.rows, &cp)
	return nil
}
func (r *memTxRepo) FindByIdem(_ context.Context, piID, adapter, idem string) (*domain.AcquirerTx, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.PiID == piID && x.Adapter == adapter && x.IdempotencyKey == idem {
			cp := *x
			return &cp, nil
		}
	}
	return nil, nil
}
func (r *memTxRepo) FindByID(_ context.Context, piID, aqID string) (*domain.AcquirerTx, error) {
	return nil, domain.ErrAcquirerTxNotFound
}
func (r *memTxRepo) UpdateResult(_ context.Context, _ string, id uint64, fields map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.ID != id {
			continue
		}
		if v, ok := fields["state"].(string); ok {
			x.State = domain.AcquirerTxState(v)
		}
		if v, ok := fields["external_ref_no"].(string); ok {
			x.ExternalRefNo = v
		}
		if v, ok := fields["failure_code"].(string); ok {
			x.FailureCode = v
		}
		if v, ok := fields["raw_failure_code"].(string); ok {
			x.RawFailureCode = v
		}
		if v, ok := fields["response_body"].(string); ok {
			x.ResponseBody = v
		}
		if v, ok := fields["latency_ms"].(int); ok {
			x.LatencyMs = v
		}
		return nil
	}
	return nil
}
func (r *memTxRepo) ListPendingRetries(_ context.Context, limit int) ([]*domain.AcquirerTx, error) {
	return nil, nil
}
func (r *memTxRepo) ListUnknownTxs(_ context.Context, limit int, olderThan, queryThrottle time.Duration) ([]*domain.AcquirerTx, error) {
	return nil, nil
}
func (r *memTxRepo) ListStuckPending(_ context.Context, limit int, stuckAfter time.Duration) ([]*domain.AcquirerTx, error) {
	return nil, nil
}

type memIDIssuer struct {
	mu sync.Mutex
	n  int64
}

func (i *memIDIssuer) Next(prefix, piID string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.n++
	return prefix + "_" + piID, nil
}
