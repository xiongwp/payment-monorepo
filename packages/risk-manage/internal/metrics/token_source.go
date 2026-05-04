// token_source.go: admin bearer token 的"来源"抽象 + hot-reload 实现。
//
// 静态 map 不够商业化：
//   - token 轮换时不能重启服务（中断 API 调用）
//   - 多副本部署，每个 pod 都得拉到最新 token，不能依赖 ConfigMap reload
//   - KMS / Vault 集成只能动态 pull
//
// TokenSource 是个 atomic-snapshot 模式：Tokens() 永远 O(1) 拿到当前 set。
// 实现：
//   - StaticTokenSource: 启动期固定（兼容老 AdminAuth）
//   - FileTokenSource:    每 N 秒读一遍 file，每行一条 token；# 注释 / 空行忽略
//   - KMSTokenSource:     调用方实现，本包不携带 KMS SDK 依赖（接口已留）

package metrics

import (
	"bufio"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// TokenSource 拿当前合法 admin token 集。Tokens() 必须无阻塞、O(1)。
type TokenSource interface {
	Tokens() map[string]struct{}
}

// StaticTokenSource 启动期固定 token 集；不可变。
type StaticTokenSource struct {
	tokens map[string]struct{}
}

func NewStaticTokenSource(tokens []string) *StaticTokenSource {
	m := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t != "" {
			m[t] = struct{}{}
		}
	}
	return &StaticTokenSource{tokens: m}
}

func (s *StaticTokenSource) Tokens() map[string]struct{} { return s.tokens }

// FileTokenSource 从 file 加载 token，每 reload 间隔重新读。
//
// 文件格式：每行一个 token；# 起头是注释；空行忽略。文件不存在 = 空 set
// （middleware 会回落 NoOp，等价 dev mode；这是开发友好的 default，
// 生产应确保文件存在 + 部署 readiness check 检查）。
type FileTokenSource struct {
	path     string
	interval time.Duration
	logger   *zap.Logger
	cache    atomic.Pointer[map[string]struct{}]
	stop     chan struct{}
	stopOnce sync.Once
}

func NewFileTokenSource(path string, reload time.Duration, logger *zap.Logger) *FileTokenSource {
	if reload <= 0 {
		reload = 30 * time.Second
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &FileTokenSource{path: path, interval: reload, logger: logger, stop: make(chan struct{})}
	empty := map[string]struct{}{}
	s.cache.Store(&empty)
	_ = s.reload()
	go s.loop()
	return s
}

func (s *FileTokenSource) Tokens() map[string]struct{} {
	m := s.cache.Load()
	if m == nil {
		return nil
	}
	return *m
}

func (s *FileTokenSource) loop() {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := s.reload(); err != nil {
				s.logger.Warn("admin token reload failed", zap.String("path", s.path), zap.Error(err))
			}
		case <-s.stop:
			return
		}
	}
}

func (s *FileTokenSource) reload() error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	out := make(map[string]struct{}, 16)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	s.cache.Store(&out)
	s.logger.Info("admin tokens reloaded",
		zap.String("path", s.path), zap.Int("count", len(out)))
	return nil
}

// Stop 优雅停 reload goroutine。fx OnStop hook 调一下即可。
func (s *FileTokenSource) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

// AdminAuthFromSource hot-reloadable 版本的 AdminAuth。每次请求查 source
// 当前快照，所以 token 轮换 / 撤销 < interval 内生效。空快照 → 仍走"完全放行"
// fallback，跟 StaticTokenSource(nil) 行为一致。
func AdminAuthFromSource(src TokenSource) func(http.Handler) http.Handler {
	if src == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokens := src.Tokens()
			if len(tokens) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			h := r.Header.Get("Authorization")
			const p = "Bearer "
			if len(h) <= len(p) || h[:len(p)] != p {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			tok := h[len(p):]
			ok := false
			for k := range tokens {
				if subtle.ConstantTimeCompare([]byte(tok), []byte(k)) == 1 {
					ok = true
					break
				}
			}
			if !ok {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
