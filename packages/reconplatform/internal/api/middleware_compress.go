// middleware_compress.go — HTTP gzip + ETag + simple cache headers.
//
// 收益:
//   - admin web HTML 页 ~ 60 KB → gzip ~ 12 KB (5x).
//   - /api/v1/scripts JSON list 100 条规则 ~ 200 KB → gzip ~ 25 KB.
//   - 静态 HTML 加 ETag, 二刷返 304 Not Modified (空 body).
//
// 应用范围:
//   - GET 请求 + 客户端 Accept-Encoding 含 gzip 才压
//   - Content-Length > 1KB 才压 (小响应不值得 CPU)
//   - SSE 流 (text/event-stream) 不压 (流式不能 gzip block)
package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
)

// WithCompression 包装一个 handler,GET 响应自动 gzip + ETag.
//
// **重要**: SSE / streaming 路径必须 bypass — recordingResponseWriter 不实现
// http.Flusher, SSE handler 会拒绝 streaming 并 500 "streaming not supported".
// 已知 SSE 路径前缀:
//   - /api/v1/events/stream   (binlog 实时流)
//   - /api/v1/diffs/stream    (将来可能)
// 任一前缀匹配 → 直接透传 underlying ResponseWriter, 保 http.Flusher 不丢.
func WithCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 非 GET / HEAD 直接透传 (POST 响应一般小)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		// SSE 路径 bypass — 保 http.Flusher
		if isSSEPath(r.URL.Path) || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			next.ServeHTTP(w, r)
			return
		}
		// 客户端不支持 gzip → 透传
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		// 缓冲响应 → 压完 + 算 ETag 后一次性写
		buf := bufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufferPool.Put(buf)

		rec := &recordingResponseWriter{
			ResponseWriter: w,
			buf:            buf,
			status:         200,
			header:         http.Header{},
		}
		next.ServeHTTP(rec, r)

		// SSE 等流式响应 跳过 (流式压会破坏分帧)
		ct := rec.header.Get("Content-Type")
		if strings.Contains(ct, "text/event-stream") {
			rec.flushPassthrough(w)
			return
		}

		body := buf.Bytes()
		// 太小不压
		if len(body) < 1024 {
			rec.flushPassthrough(w)
			return
		}

		// ETag 用 sha256[:8] (够防碰撞)
		h := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(h[:8]) + `"`

		// If-None-Match → 304
		if r.Header.Get("If-None-Match") == etag {
			copyHeaders(w.Header(), rec.header)
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}

		// gzip + 写
		gzBuf := bufferPool.Get().(*bytes.Buffer)
		gzBuf.Reset()
		defer bufferPool.Put(gzBuf)
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(gzBuf)
		_, _ = gz.Write(body)
		_ = gz.Close()
		gzipWriterPool.Put(gz)

		copyHeaders(w.Header(), rec.header)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("ETag", etag)
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length") // gzip 后 length 变了
		w.WriteHeader(rec.status)
		_, _ = w.Write(gzBuf.Bytes())
	})
}

// recordingResponseWriter 拦截 Write,把响应缓冲下来供 gzip / ETag 处理.
type recordingResponseWriter struct {
	http.ResponseWriter
	buf    *bytes.Buffer
	status int
	header http.Header
}

func (r *recordingResponseWriter) Header() http.Header { return r.header }

func (r *recordingResponseWriter) WriteHeader(code int) { r.status = code }

func (r *recordingResponseWriter) Write(b []byte) (int, error) {
	return r.buf.Write(b)
}

// flushPassthrough 把缓冲的 header + body 原样写到底层 ResponseWriter.
func (r *recordingResponseWriter) flushPassthrough(w http.ResponseWriter) {
	copyHeaders(w.Header(), r.header)
	w.WriteHeader(r.status)
	_, _ = io.Copy(w, r.buf)
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = v
	}
}

// isSSEPath SSE 端点列表 (硬编码避免每请求查全局 map).
func isSSEPath(path string) bool {
	return strings.HasPrefix(path, "/api/v1/events/stream") ||
		strings.HasPrefix(path, "/api/v1/diffs/stream")
}

// 缓冲池 (减少 GC 压力).
var bufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}
var gzipWriterPool = sync.Pool{
	New: func() any {
		gz, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return gz
	},
}
