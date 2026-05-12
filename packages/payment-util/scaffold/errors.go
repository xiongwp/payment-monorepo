// errors.go — RFC 7807 Problem+JSON 统一错误格式.
//
// 所有 4xx / 5xx 走 application/problem+json:
//   {
//     "type":     "https://docs.example.com/errors/conflict",
//     "title":    "Resource Conflict",
//     "status":   409,
//     "detail":   "request_id already exists with different payload",
//     "instance": "/v1/requests/dsar_xyz",
//     "trace_id": "..."  // 我们扩展
//   }

package scaffold

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

// Problem RFC 7807 + 扩展.
type Problem struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`

	// 扩展
	Code      string `json:"code,omitempty"`       // 内部 error code
	TraceID   string `json:"trace_id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// ErrorWith 业务侧手动返 problem+json.
//
//   return scaffold.ErrorWith(c, 409, "conflict", "request_id taken")
func ErrorWith(c *fiber.Ctx, status int, code, detail string) error {
	c.Set("Content-Type", "application/problem+json")
	return c.Status(status).JSON(Problem{
		Title:     httpStatusTitle(status),
		Status:    status,
		Detail:    detail,
		Code:      code,
		Instance:  c.Path(),
		RequestID: c.GetRespHeader("X-Request-Id"),
	})
}

// newErrorHandler fiber 全局错误兜底 — 任何 panic / 500 / fiber.NewError 都走这里.
func newErrorHandler(log *zap.Logger) fiber.ErrorHandler {
	return func(c *fiber.Ctx, err error) error {
		status := fiber.StatusInternalServerError
		code := "internal_error"
		detail := err.Error()

		var fe *fiber.Error
		if errors.As(err, &fe) {
			status = fe.Code
			detail = fe.Message
			code = httpStatusCode(status)
		}

		if status >= 500 {
			log.Error("http 5xx",
				zap.String("path", c.Path()),
				zap.String("method", c.Method()),
				zap.Error(err))
		}

		c.Set("Content-Type", "application/problem+json")
		return c.Status(status).JSON(Problem{
			Title:     httpStatusTitle(status),
			Status:    status,
			Detail:    detail,
			Code:      code,
			Instance:  c.Path(),
			RequestID: c.GetRespHeader("X-Request-Id"),
		})
	}
}

func httpStatusTitle(s int) string {
	switch s {
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 429:
		return "Too Many Requests"
	case 500:
		return "Internal Server Error"
	case 503:
		return "Service Unavailable"
	}
	return "HTTP Error"
}

func httpStatusCode(s int) string {
	switch s {
	case 400:
		return "bad_request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 422:
		return "unprocessable"
	case 429:
		return "rate_limited"
	case 500:
		return "internal_error"
	case 503:
		return "unavailable"
	}
	return "http_error"
}
