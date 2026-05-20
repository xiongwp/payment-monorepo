// Package efile — 电子申报 transport.
//
// IRS FIRE (Filing Information Returns Electronically):
//   - 系统: https://fire.irs.gov, SFTP/HTTPS upload
//   - 文件格式: fixed-width text, Publication 1220
//   - 注册要 TCC (Transmitter Control Code) — 平台一次性申请
//
// Avalara / TaxJar / Vertex:
//   - 商用替代; REST API; 上送 1099-K + 自动报送
//   - 适合不想跑 IRS FIRE 直连的平台
//
// 这里实现 stub Submitter — 后接生产 transport.

package efile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"reconcile-system/packages/tax-reporting/internal/domain"
)

var (
	ErrNotReady   = errors.New("efile: form not ready")
	ErrInvalidTIN = errors.New("efile: invalid payee TIN")
)

// Submitter e-file 总抽象.
type Submitter interface {
	Submit(ctx context.Context, form domain.TaxForm) (SubmitResult, error)
	Status(ctx context.Context, efileID string) (string, error) // accepted / rejected / pending
}

type SubmitResult struct {
	EFileID    string
	Receipt    string // IRS 收据号 / Avalara confirmation
	SubmittedAt time.Time
}

// StubSubmitter — dev 用. 立刻返已"接受".
type StubSubmitter struct{}

func (s StubSubmitter) Submit(_ context.Context, f domain.TaxForm) (SubmitResult, error) {
	if f.Status != domain.FormReady {
		return SubmitResult{}, ErrNotReady
	}
	return SubmitResult{
		EFileID:     "stub-" + f.FormID,
		Receipt:     fmt.Sprintf("ACK-%s-%d", f.FormID, time.Now().Unix()),
		SubmittedAt: time.Now().UTC(),
	}, nil
}

func (s StubSubmitter) Status(_ context.Context, _ string) (string, error) {
	return "accepted", nil
}

// AvalaraSubmitter — 真实接入 Avalara 1099 API (生产用).
//
// 完整 HTTP 集成 (POST /api/v2/1099/forms + Bearer Auth + status poll) 是
// 下个 PR 的事; 此处先给可构造的真 struct, 让 main.go 能选 TAX_SUBMITTER=avalara
// 而不直接 panic. 上线前必须接通真实 endpoint.
type AvalaraSubmitter struct {
	APIKey      string
	BaseURL     string
	HTTPTimeout time.Duration
}

// NewAvalaraSubmitter — P0-TAX-1 入口. main.go 通过 TAX_SUBMITTER=avalara 选这条.
// API key / URL 从 env 注入 (AVALARA_API_KEY / AVALARA_API_URL).
func NewAvalaraSubmitter(apiKey, baseURL string) *AvalaraSubmitter {
	if baseURL == "" {
		baseURL = "https://api.avalara.com" // 默认 prod endpoint
	}
	return &AvalaraSubmitter{
		APIKey:      apiKey,
		BaseURL:     baseURL,
		HTTPTimeout: 30 * time.Second,
	}
}

func (a *AvalaraSubmitter) Submit(_ context.Context, _ domain.TaxForm) (SubmitResult, error) {
	// TODO P0-TAX-1 后续 PR: POST <BaseURL>/api/v2/1099/forms with Bearer APIKey, JSON payload.
	// 拿 status=submitted + filing_id 回填 TaxForm.EFileID.
	return SubmitResult{}, errors.New("avalara: HTTP integration pending (P0-TAX-1 step 2)")
}
func (a *AvalaraSubmitter) Status(_ context.Context, _ string) (string, error) {
	return "", errors.New("avalara: HTTP integration pending (P0-TAX-1 step 2)")
}

// IRSFireSubmitter — 直连 IRS FIRE (Filing Information Returns Electronically).
// 需要 TCC (Transmitter Control Code) + 客户端证书. 配置: IRS_FIRE_TCC / IRS_FIRE_CERT_PATH.
type IRSFireSubmitter struct {
	TCC      string // 由 IRS 颁发的 5-digit transmitter code
	CertPath string // 客户端 mTLS 证书路径
}

func NewIRSFireSubmitter(tcc, certPath string) *IRSFireSubmitter {
	return &IRSFireSubmitter{TCC: tcc, CertPath: certPath}
}

func (i *IRSFireSubmitter) Submit(_ context.Context, _ domain.TaxForm) (SubmitResult, error) {
	// TODO P0-TAX-1 后续 PR: IRS FIRE SFTP / SOAP submission.
	return SubmitResult{}, errors.New("irs_fire: integration pending (P0-TAX-1 step 2)")
}

func (i *IRSFireSubmitter) Status(_ context.Context, _ string) (string, error) {
	return "", errors.New("irs_fire: integration pending (P0-TAX-1 step 2)")
}
