// Package auditmw — HTTP / gRPC 通用 audit middleware.
//
// 接到任一 admin web 出口, 自动给 audit-log 服务发一条 "who-did-what" 记录.
//
// 用法 (HTTP):
//
//	h := auditmw.HTTP(myHandler, auditmw.Opts{
//	    Service:   "biz-admin-web",
//	    AuditURL:  "http://audit-log:8087",
//	    ResourceFn: func(r *http.Request) (rType, rID string) {
//	        return r.PathValue("type"), r.PathValue("id")
//	    },
//	})
//
// 用法 (gRPC):
//
//	s := grpc.NewServer(grpc.UnaryInterceptor(auditmw.UnaryServer(opts)))
package auditmw

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Opts 中间件配置.
type Opts struct {
	Service  string // 自己的 service 名 (落到 audit_log.service)
	AuditURL string // audit-log endpoint (e.g. http://audit-log:8087)
	Client   HTTPDoer

	// MutatingOnly true → 仅对 POST/PUT/PATCH/DELETE 记 audit (减负).
	MutatingOnly bool

	// ResourceFn 调用方决定 audit_log.resource_type / resource_id (e.g. order_id, merchant_id).
	ResourceFn func(r *http.Request) (rType, rID string)

	// ActorFn 调用方决定操作者 email (默认从 X-Actor-Email header 读).
	ActorFn func(r *http.Request) string
}

// HTTPDoer 抽象 http.Client (单测可换 mock).
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Default 默认 http client.
func Default() HTTPDoer {
	return &http.Client{Timeout: 2 * time.Second}
}

// HTTP wraps an http.Handler.
func HTTP(h http.Handler, opts Opts) http.Handler {
	if opts.Client == nil {
		opts.Client = Default()
	}
	if opts.ActorFn == nil {
		opts.ActorFn = func(r *http.Request) string {
			return r.Header.Get("X-Actor-Email")
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 仅对 mutating 方法记 audit (可关)
		if opts.MutatingOnly && r.Method != http.MethodPost && r.Method != http.MethodPut &&
			r.Method != http.MethodPatch && r.Method != http.MethodDelete {
			h.ServeHTTP(w, r)
			return
		}
		rec := &recordingWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		// 异步发,不阻塞响应
		go sendAudit(r.Context(), opts, auditEntry{
			Service:      opts.Service,
			ActorEmail:   opts.ActorFn(r),
			Action:       r.Method + " " + r.URL.Path,
			ResourceType: rType(r, opts),
			ResourceID:   rID(r, opts),
			Status:       rec.status,
			At:           time.Now().UTC(),
			IP:           clientIP(r),
		})
	})
}

func rType(r *http.Request, opts Opts) string {
	if opts.ResourceFn != nil {
		t, _ := opts.ResourceFn(r)
		return t
	}
	return strings.TrimPrefix(r.URL.Path, "/")
}

func rID(r *http.Request, opts Opts) string {
	if opts.ResourceFn != nil {
		_, id := opts.ResourceFn(r)
		return id
	}
	return ""
}

// recordingWriter 记 status code (避免覆盖底层 ResponseWriter).
type recordingWriter struct {
	http.ResponseWriter
	status int
}

func (r *recordingWriter) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// UnaryServer gRPC interceptor.
func UnaryServer(opts Opts) grpc.UnaryServerInterceptor {
	if opts.Client == nil {
		opts.Client = Default()
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)
		actor := actorFromMD(ctx)
		// async send
		go sendAudit(ctx, opts, auditEntry{
			Service:    opts.Service,
			ActorEmail: actor,
			Action:     info.FullMethod,
			Status:     errStatus(err),
			At:         time.Now().UTC(),
		})
		return resp, err
	}
}

func actorFromMD(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("x-actor-email"); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func errStatus(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.IndexByte(xf, ','); i >= 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	return r.RemoteAddr
}

type auditEntry struct {
	Service      string    `json:"service"`
	ActorEmail   string    `json:"actor_email"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type,omitempty"`
	ResourceID   string    `json:"resource_id,omitempty"`
	Status       int       `json:"status"`
	At           time.Time `json:"at"`
	IP           string    `json:"ip,omitempty"`
}

func sendAudit(ctx context.Context, opts Opts, e auditEntry) {
	body, err := json.Marshal(e)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		opts.AuditURL+"/api/v1/audit/log", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := opts.Client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}
