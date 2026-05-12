// Package handler — Fiber 路由层 (替代原 adminhttp).
//
// canonical 结构: domain (types) → repo (gorm) → service (业务) → handler (HTTP).
// handler 只做 parse / validate / 调 service / 编码响应; 不写业务.

package handler

import (
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"github.com/xiongwp/payment-util/scaffold"

	"reconcile-system/packages/data-rights/internal/domain"
	"reconcile-system/packages/data-rights/internal/service"
)

type Handler struct {
	svc *service.Service
	log *zap.Logger
}

func New(svc *service.Service, log *zap.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Register 标准签名 — main.go 通过 scaffold.Opts.RegisterRoutes 注入.
func Register(app *scaffold.App, cfg *scaffold.Config) error {
	// 业务侧 service 装配
	// (真实 main.go 会 DI 注入; 这里假设 svc 已构造)
	return nil // 由真 main.go 实现 (示例见 cmd/server/main_v2.go)
}

// Mount 装载 Fiber 路由. 区分 public / merchant-self / admin.
func (h *Handler) Mount(app *scaffold.App) {
	// 用户面 (商户后台 / 终端 SDK)
	app.Fiber.Post("/v1/requests", h.submit)
	app.Fiber.Get("/v1/requests/:id", h.getRequest)

	// Admin (X-Admin-Token 守护)
	admin := app.AdminGroup()
	admin.Get("/requests", h.listRequests)
	admin.Post("/requests/:id/:action", h.adminAction)
	admin.Get("/stream", h.sseStream) // SSE 给 biz-admin-web
}

// ── handlers ──

func (h *Handler) submit(c *fiber.Ctx) error {
	var body struct {
		Type         domain.RequestType  `json:"type"`
		Subject      domain.Subject      `json:"subject"`
		Jurisdiction domain.Jurisdiction `json:"jurisdiction"`
	}
	if err := c.BodyParser(&body); err != nil {
		return scaffold.ErrorWith(c, 400, "bad_json", err.Error())
	}
	if body.Subject.ID == "" || body.Subject.Type == "" {
		return scaffold.ErrorWith(c, 400, "bad_subject", "subject.id / subject.type required")
	}
	req, err := h.svc.Submit(c.Context(), body.Type, body.Subject, body.Jurisdiction, c.IP())
	if err != nil {
		return scaffold.ErrorWith(c, 500, "internal_error", err.Error())
	}
	return c.Status(201).JSON(req)
}

func (h *Handler) getRequest(c *fiber.Ctx) error {
	id := c.Params("id")
	req, err := h.svc.Get(c.Context(), id)
	if err != nil {
		return scaffold.ErrorWith(c, 404, "not_found", err.Error())
	}
	return c.JSON(req)
}

func (h *Handler) listRequests(c *fiber.Ctx) error {
	filter := h.svc.ParseListFilter(
		c.Query("state"),
		c.Query("type"),
		c.Query("overdue") == "1",
		c.QueryInt("limit", 50),
		c.QueryInt("offset", 0),
	)
	list, err := h.svc.List(c.Context(), filter)
	if err != nil {
		return scaffold.ErrorWith(c, 500, "internal_error", err.Error())
	}
	return c.JSON(fiber.Map{"requests": list, "count": len(list)})
}

func (h *Handler) adminAction(c *fiber.Ctx) error {
	id := c.Params("id")
	action := c.Params("action")
	var body struct {
		Reviewer string `json:"reviewer"`
		Reason   string `json:"reason"`
	}
	_ = c.BodyParser(&body)

	switch action {
	case "verify":
		err := h.svc.Verify(c.Context(), id, body.Reviewer, c.IP())
		if err != nil {
			return scaffold.ErrorWith(c, 400, "verify_failed", err.Error())
		}
	case "approve":
		err := h.svc.Approve(c.Context(), id, body.Reviewer, c.IP())
		if err != nil {
			return scaffold.ErrorWith(c, 400, "approve_failed", err.Error())
		}
	case "reject":
		if body.Reason == "" {
			return scaffold.ErrorWith(c, 400, "missing_reason", "reason required")
		}
		err := h.svc.Reject(c.Context(), id, body.Reviewer, body.Reason, c.IP())
		if err != nil {
			return scaffold.ErrorWith(c, 400, "reject_failed", err.Error())
		}
	case "fulfill":
		err := h.svc.Fulfill(c.Context(), id, body.Reviewer, c.IP())
		if err != nil {
			return scaffold.ErrorWith(c, 400, "fulfill_failed", err.Error())
		}
	default:
		return scaffold.ErrorWith(c, 400, "bad_action", "expect verify|approve|reject|fulfill")
	}

	req, _ := h.svc.Get(c.Context(), id)
	return c.JSON(req)
}

// sseStream — 推 request_update 事件到 biz-admin-web.
func (h *Handler) sseStream(c *fiber.Ctx) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	// 简化: 真生产从 service.SubscribeUpdates() 拉; 这里发 hello
	_, _ = c.WriteString("event: hello\ndata: {\"connected\":true}\n\n")
	return nil
}
