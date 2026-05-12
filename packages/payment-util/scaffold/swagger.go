// swagger.go — 自动暴露 /openapi.json + /docs (swagger-ui CDN).
//
// 业务侧:
//   scaffold.Run(scaffold.Opts{
//       ...
//       OpenAPISpec: openapiYAMLBytes,    // 业务侧 embed
//       OpenAPIInfo: scaffold.OpenAPIInfo{Title: "..", Version: ".."},
//   })
//
// 自动:
//   GET /openapi.json  →  返 spec (JSON 或 YAML 都 OK; 自动检测)
//   GET /docs          →  swagger-ui HTML (CDN, 零依赖)
//
// 真生产建议:
//   - spec 走 oapi-codegen 生成 Go types + handlers
//   - 商户 dev 在 https://api.<domain>/docs 看交互式 explorer
//   - SDK 自动 codegen 从同一 spec (5 语言 SDK 已有, 后续可换 codegen)

package scaffold

import (
	"github.com/gofiber/fiber/v2"
)

// OpenAPIInfo embed 时填.
type OpenAPIInfo struct {
	Title       string
	Version     string
	Description string
}

// MountSwagger 装 /openapi.{json,yaml} + /docs.
// 由 NewApp / Run 在业务侧 RegisterRoutes 后调.
func MountSwagger(app *App, spec []byte, info OpenAPIInfo) {
	if len(spec) == 0 {
		return
	}
	// spec content-type 检测
	ct := "application/json"
	for _, b := range spec[:min(len(spec), 200)] {
		if b == '{' {
			ct = "application/json"
			break
		}
		if b > ' ' {
			ct = "application/yaml"
			break
		}
	}

	app.Fiber.Get("/openapi.json", func(c *fiber.Ctx) error {
		c.Set("Content-Type", ct)
		return c.Send(spec)
	})
	app.Fiber.Get("/openapi.yaml", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "application/yaml")
		return c.Send(spec)
	})

	// swagger-ui HTML — CDN 加载, 零本地 deps
	title := info.Title
	if title == "" {
		title = app.Cfg.ServiceName + " API"
	}
	html := `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>` + title + `</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css">
<style>html,body{margin:0;background:#fafafa;}</style>
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-standalone-preset.js"></script>
<script>
window.onload = () => {
  SwaggerUIBundle({
    url: "/openapi.json",
    dom_id: "#swagger-ui",
    deepLinking: true,
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
    layout: "StandaloneLayout",
    persistAuthorization: true,
    tryItOutEnabled: true,
  });
};
</script>
</body>
</html>`

	app.Fiber.Get("/docs", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.SendString(html)
	})

	// Redoc 替代 — 更适合 read-only 文档场景
	redoc := `<!DOCTYPE html>
<html>
<head><title>` + title + `</title><meta charset="utf-8"></head>
<body>
<redoc spec-url="/openapi.json"></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"></script>
</body>
</html>`
	app.Fiber.Get("/redoc", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.SendString(redoc)
	})
}
