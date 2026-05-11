// new-service — payment-monorepo 新微服务脚手架生成器.
//
// 用:
//   go run ./tools/new-service \
//     -name fraud-detection \
//     -port 8108 \
//     -domain fraud
//
// 产出 (在 packages/{name}/ 下):
//   cmd/server/main.go               基于 mw.Bootstrap, OAuth2/metrics/healthz/tracing 现成
//   go.mod                           监 payment-mw + payment-util
//   Dockerfile                       multi-stage
//   internal/domain/types.go         空骨架 (按 domain 命名)
//   internal/repo/memory.go          MemoryRepo 占位
//   internal/adminhttp/server.go     /api/v1/{domain}/{...}
//   README.md                        填了 service 元数据
//   deploy/k8s/{name}.yaml           Deployment/Service/HPA/PDB
//   api/openapi.yaml                 OpenAPI 3.0 骨架 (含 securitySchemes)
//
// 跑完后:
//   1. cd packages/{name}
//   2. go mod tidy
//   3. docker build -t {name}:local .
//   4. 自动加进 go.work + biz-stack docker-compose 后即可起

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

var (
	name    = flag.String("name", "", "service name (kebab-case, e.g. fraud-detection)")
	port    = flag.String("port", "8100", "HTTP port")
	domain  = flag.String("domain", "", "API domain prefix (default = name)")
	out     = flag.String("out", "packages", "output base dir (relative to repo root)")
	dryRun  = flag.Bool("dry-run", false, "show what would be generated, do not write files")
)

// 模板上下文.
type ctx struct {
	ServiceName   string // "fraud-detection"
	ServiceUpper  string // "FRAUD_DETECTION"
	ServicePascal string // "FraudDetection"
	DefaultPort   string // "8100"
	Domain        string // "fraud"
}

func main() {
	flag.Parse()
	if *name == "" {
		fatal("required: -name")
	}
	if !isValidName(*name) {
		fatal("name must be kebab-case lowercase ASCII (e.g. fraud-detection)")
	}
	d := *domain
	if d == "" {
		d = *name
	}

	c := ctx{
		ServiceName:   *name,
		ServiceUpper:  strings.ToUpper(strings.ReplaceAll(*name, "-", "_")),
		ServicePascal: toPascal(*name),
		DefaultPort:   *port,
		Domain:        d,
	}

	root := filepath.Join(*out, *name)
	if _, err := os.Stat(root); err == nil {
		fatal(root + " already exists; abort")
	}

	files := map[string]string{
		"cmd/server/main.go":         tplMain,
		"go.mod":                     tplGoMod,
		"Dockerfile":                 tplDockerfile,
		"internal/domain/types.go":   tplDomain,
		"internal/repo/memory.go":    tplRepo,
		"internal/adminhttp/server.go": tplHTTP,
		"README.md":                  tplReadme,
		"deploy/k8s/" + *name + ".yaml": tplK8s,
		"api/openapi.yaml":           tplOpenAPI,
	}

	fmt.Printf("Generating service: %s\n", *name)
	for relPath, raw := range files {
		full := filepath.Join(root, relPath)
		rendered := render(raw, c)
		if *dryRun {
			fmt.Printf("  [dry] %s (%d bytes)\n", full, len(rendered))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			fatal(err.Error())
		}
		if err := os.WriteFile(full, []byte(rendered), 0o644); err != nil {
			fatal(err.Error())
		}
		fmt.Printf("  ✓ %s\n", full)
	}
	if !*dryRun {
		fmt.Printf("\n✅ %s scaffolded. Next:\n", *name)
		fmt.Printf("  cd %s && go mod tidy\n", root)
		fmt.Printf("  把 ./%s 加进 go.work\n", root)
		fmt.Printf("  docker build -t %s:local .\n", *name)
	}
}

func render(src string, c ctx) string {
	t, err := template.New("").Parse(src)
	if err != nil {
		fatal(err.Error())
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, c); err != nil {
		fatal(err.Error())
	}
	return buf.String()
}

func isValidName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

func toPascal(s string) string {
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(string(p[0])) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "ERROR:", msg)
	os.Exit(1)
}

// ─── 模板 (inlined; 跟 template/ 目录里的 .tmpl 双轨 — 这里更易维护) ─────

const tplMain = `package main

import (
	"context"
	"net/http"

	mw "reconcile-system/packages/payment-mw"

	"go.uber.org/zap"
)

func main() {
	mw.Bootstrap(mw.BootstrapConfig{
		ServiceName: "{{.ServiceName}}",
		DefaultPort: "{{.DefaultPort}}",
		AuthCfg: mw.AuthConfig{
			PublicPaths: []string{"/healthz", "/metrics"},
		},
		SetupRoutes: func(mux *http.ServeMux, log *zap.Logger) error {
			// TODO: 业务路由 — 一例:
			// mux.Handle("POST /api/v1/{{.Domain}}",
			//     mw.RequireScope("{{.Domain}}:write")(http.HandlerFunc(create{{.ServicePascal}})))
			return nil
		},
		OnStart: func(ctx context.Context, log *zap.Logger) error {
			log.Info("{{.ServiceName}} ready")
			return nil
		},
	})
}
`

const tplGoMod = `module reconcile-system/packages/{{.ServiceName}}

go 1.22

require (
	go.uber.org/zap v1.27.0
	reconcile-system/packages/payment-mw v0.0.0
	reconcile-system/packages/payment-util v0.0.0
)

replace reconcile-system/packages/payment-mw => ../payment-mw
replace reconcile-system/packages/payment-util => ../payment-util
`

const tplDockerfile = `FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/{{.ServiceName}} ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/{{.ServiceName}} /app/{{.ServiceName}}
USER app
EXPOSE {{.DefaultPort}}
ENTRYPOINT ["/app/{{.ServiceName}}"]
`

const tplDomain = `package domain

import "time"

type {{.ServicePascal}} struct {
	ID        int64     ` + "`db:\"id\" json:\"id\"`" + `
	Status    string    ` + "`db:\"status\" json:\"status\"`" + `
	CreatedAt time.Time ` + "`db:\"created_at\" json:\"created_at\"`" + `
}
`

const tplRepo = `package repo

import (
	"context"
	"sync"

	"reconcile-system/packages/{{.ServiceName}}/internal/domain"
)

type MemoryRepo struct {
	mu   sync.RWMutex
	data map[int64]*domain.{{.ServicePascal}}
	next int64
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{data: map[int64]*domain.{{.ServicePascal}}{}}
}

func (m *MemoryRepo) Save(_ context.Context, x *domain.{{.ServicePascal}}) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	x.ID = m.next
	m.data[x.ID] = x
	return x.ID, nil
}

func (m *MemoryRepo) Get(_ context.Context, id int64) (*domain.{{.ServicePascal}}, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.data[id], nil
}
`

const tplHTTP = `package adminhttp

import (
	"net/http"

	"reconcile-system/packages/{{.ServiceName}}/internal/repo"
)

type Server struct {
	Repo *repo.MemoryRepo
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/{{.Domain}}", s.handleList)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(` + "`{\"data\":[]}`" + `))
}
`

const tplReadme = `# {{.ServiceName}}

Auto-generated by ` + "`tools/new-service`" + `.

## Run

` + "```bash" + `
go run ./cmd/server
` + "```" + `

## Env

- {{.ServiceUpper}}_HTTP_PORT (default {{.DefaultPort}})
- OAUTH2_JWKS_URL (enable Bearer JWT)
- ACCOUNTING_GRPC_ADDR

## API

- GET  /healthz
- GET  /metrics
- /api/v1/{{.Domain}}/*

## TODO

- [ ] 加业务路由 (cmd/server/main.go SetupRoutes)
- [ ] domain types (internal/domain/types.go)
- [ ] MySQL repo (复制 billing-system 模板)
- [ ] OpenAPI spec (api/openapi.yaml)
- [ ] k8s 加进 biz-stack
- [ ] 加 go.work use 行
- [ ] k6 性能基准 (test/k6/)
`

const tplK8s = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{.ServiceName}}
  namespace: payment
spec:
  replicas: 2
  selector: { matchLabels: { app: {{.ServiceName}} } }
  template:
    metadata:
      labels: { app: {{.ServiceName}}, chaos-eligible: "true" }
    spec:
      containers:
      - name: {{.ServiceName}}
        image: {{.ServiceName}}:latest
        ports: [{ containerPort: {{.DefaultPort}} }]
        readinessProbe:
          httpGet: { path: /healthz, port: {{.DefaultPort}} }
        resources:
          requests: { cpu: "100m", memory: "128Mi" }
          limits:   { cpu: "1",    memory: "512Mi" }
---
apiVersion: v1
kind: Service
metadata: { name: {{.ServiceName}}, namespace: payment }
spec:
  selector: { app: {{.ServiceName}} }
  ports: [{ port: {{.DefaultPort}}, targetPort: {{.DefaultPort}} }]
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: { name: {{.ServiceName}}, namespace: payment }
spec:
  scaleTargetRef: { apiVersion: apps/v1, kind: Deployment, name: {{.ServiceName}} }
  minReplicas: 2
  maxReplicas: 10
  metrics:
  - type: Resource
    resource: { name: cpu, target: { type: Utilization, averageUtilization: 70 } }
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: { name: {{.ServiceName}}, namespace: payment }
spec:
  minAvailable: 1
  selector: { matchLabels: { app: {{.ServiceName}} } }
`

const tplOpenAPI = `openapi: 3.0.3
info:
  title: {{.ServicePascal}} API
  version: 1.0.0
servers:
  - url: http://{{.ServiceName}}:{{.DefaultPort}}
security:
  - oauth2BearerJWT: [{{.Domain}}:read]
paths:
  /healthz:
    get: { summary: liveness, security: [], responses: { '200': { description: ok } } }
  /api/v1/{{.Domain}}:
    get:
      summary: List
      responses: { '200': { description: ok } }
components:
  securitySchemes:
    oauth2BearerJWT: { $ref: '../../../api/openapi/_common.yaml#/components/securitySchemes/oauth2BearerJWT' }
`
