// Package notifier — DSAR fulfill 邮件通知 (P0-DSAR-1).
//
// 实现 adminhttp.Notifier interface. 收到 fulfilled 信号后给用户发邮件
// (含 7d 过期下载链接). 真接邮件 service (SES / SendGrid / SMTP) 在
// 下个 PR; 当前给 interface + LogNotifier (dev) + SMTPNotifier (skeleton).
package notifier

import (
	"context"
	"fmt"
	"net/smtp"

	"go.uber.org/zap"

	"reconcile-system/packages/data-rights/internal/domain"
)

// LogNotifier — dev 用. 把邮件内容打 log, 不真发.
type LogNotifier struct {
	Log *zap.Logger
}

func (n *LogNotifier) NotifyFulfilled(_ context.Context, req *domain.Request, url string) error {
	if n.Log != nil {
		n.Log.Info("DSAR fulfilled — would email user (dev mode)",
			zap.String("request_id", req.RequestID),
			zap.String("subject_id", req.Subject.ID),
			zap.String("type", string(req.Type)),
			zap.String("download_url", url))
	}
	return nil
}

// SMTPNotifier — prod 用. 通过 SMTP relay 发邮件.
//
// 用法 (main.go):
//
//	n := &notifier.SMTPNotifier{
//	    Host:     os.Getenv("DSAR_SMTP_HOST"),
//	    Port:     587,
//	    Username: os.Getenv("DSAR_SMTP_USER"),
//	    Password: os.Getenv("DSAR_SMTP_PASS"),
//	    From:     "noreply@example.com",
//	}
//
// AWS SES / SendGrid 后续 PR 也加在这里 (同 interface).
type SMTPNotifier struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	Log      *zap.Logger
}

func (n *SMTPNotifier) NotifyFulfilled(_ context.Context, req *domain.Request, url string) error {
	if req == nil {
		return fmt.Errorf("smtp notifier: nil request")
	}
	if req.Subject.Email == "" {
		return fmt.Errorf("smtp notifier: missing subject email for request %s", req.RequestID)
	}
	subject := "Your data export is ready"
	body := fmt.Sprintf(`Hello,

Your data subject access request (%s) has been fulfilled.

Download link (valid 7 days):
%s

This link is one-time use and encrypted at rest. If you did not request this,
please contact privacy@example.com immediately.

Regards,
Privacy Office
`, req.RequestID, url)

	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		n.From, req.Subject.Email, subject, body))

	auth := smtp.PlainAuth("", n.Username, n.Password, n.Host)
	addr := fmt.Sprintf("%s:%d", n.Host, n.Port)
	return smtp.SendMail(addr, auth, n.From, []string{req.Subject.Email}, msg)
}
