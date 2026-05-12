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

// AvalaraSubmitter — 真实接入 Avalara 1099 API (生产用; 这里给接口骨架).
type AvalaraSubmitter struct {
	APIKey      string
	BaseURL     string
	HTTPTimeout time.Duration
}

func (a *AvalaraSubmitter) Submit(_ context.Context, _ domain.TaxForm) (SubmitResult, error) {
	// 真实: POST <BaseURL>/api/v2/1099/forms with Bearer APIKey, JSON payload.
	// 拿 status=submitted + filing_id 回填 TaxForm.EFileID.
	return SubmitResult{}, errors.New("avalara: not implemented in dev")
}
func (a *AvalaraSubmitter) Status(_ context.Context, _ string) (string, error) {
	return "", errors.New("avalara: not implemented in dev")
}
