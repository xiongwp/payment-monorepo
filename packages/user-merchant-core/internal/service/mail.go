// Package service — Mailer 接口（发邮件 / 短信）。
//
// dev 默认 LogMailer：把 code 打 INFO log 让本地调试能看到。
// 生产换 SMTPMailer / SendGridMailer / TwilioMailer 等。
package service

import (
	"context"

	"go.uber.org/zap"
)

// Mailer 发邮件 / 短信验证码。
type Mailer interface {
	Send(ctx context.Context, to, purpose, code string) error
}

// LogMailer 把 (to, purpose, code) 打到日志。dev / 单测用。
type LogMailer struct{ Logger *zap.Logger }

func (m *LogMailer) Send(_ context.Context, to, purpose, code string) error {
	if m == nil || m.Logger == nil {
		return nil
	}
	m.Logger.Info("MAIL/OTP code (dev mailer)",
		zap.String("to", to),
		zap.String("purpose", purpose),
		zap.String("code", code))
	return nil
}
