// attempt_hook.go — dispatcher 接 attempt log 的扩展.
//
// 原 dispatcher.deliverOne 只更新 Delivery 行 (last-write-wins overwrite, 历史丢失).
// 这里加一个 AttemptRecorder hook, 每次 deliverOne 完成后调.
//
// 调用方注入 (dispatcher.New 后 setter):
//   d.SetAttemptRecorder(repo.AppendAttempt)

package dispatcher

import (
	"context"
	"time"

	"reconcile-system/packages/merchant-webhook/internal/domain"
)

// AttemptRecorder 由 repo 实现 (memory + mysql).
type AttemptRecorder func(ctx context.Context, a *domain.Attempt) error

// SetAttemptRecorder 注入. 没设的话 attempt 不记录 (dispatcher 仍工作, 只是商户看不到 history).
func (d *Dispatcher) SetAttemptRecorder(rec AttemptRecorder) { d.attRec = rec }

// recordAttempt 在 deliverOne 关键节点调用 (成功 / 失败 / DLQ 各一次).
func (d *Dispatcher) recordAttempt(ctx context.Context, del *domain.Delivery, sigTS int64,
	httpStatus int, durMs int64, errMsg, respBody, secretKeyID string, sent bool) {
	if d.attRec == nil {
		return
	}
	_ = d.attRec(ctx, &domain.Attempt{
		DeliveryID:   del.ID,
		AttemptNum:   del.Attempt,
		HTTPStatus:   httpStatus,
		DurationMS:   int(durMs),
		ErrorMessage: errMsg,
		ResponseBody: respBody,
		SignatureTS:  sigTS,
		RequestSent:  sent,
		SecretKeyID:  secretKeyID,
		OccurredAt:   time.Now().UTC(),
	})
}
