package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// noopFleetCache：未注入 configcenter / baseURL 时回退到 noop —
// Get 永远 miss，Push 永远成功（caller fallback DB）。
func TestFleetCache_NoopFallback(t *testing.T) {
	cache := NewFleetCache(nil, "", "actor-x", zap.NewNop())

	accountNo, hit := cache.Get(123, 7)
	assert.False(t, hit)
	assert.Equal(t, "", accountNo)

	// Push 不报错（caller 不应被退化卡死）
	err := cache.Push(context.Background(), 123, make([]string, 100))
	assert.NoError(t, err)

	// 即使 baseURL 给了但 client nil 也应退化
	cache2 := NewFleetCache(nil, "http://example.com", "actor", zap.NewNop())
	_, hit2 := cache2.Get(1, 0)
	assert.False(t, hit2)
}

// Push 走 HTTP PUT；验证 server 收到的 URL、body shape 正确。
// 不依赖真实 configcenter SDK；直接拿 mock server 来测 HTTP 行为。
func TestFleetCache_PushHTTPShape(t *testing.T) {
	var (
		mu          sync.Mutex
		gotMethod   string
		gotURLPath  string
		gotBody     map[string]interface{}
		gotJSONMap  map[string]string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotMethod = r.Method
		gotURLPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		if vRaw, ok := gotBody["value"].(string); ok {
			_ = json.Unmarshal([]byte(vRaw), &gotJSONMap)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":1}`))
	}))
	defer srv.Close()

	// 走 configFleetCache 内部 HTTP path；cli 给个虚的（Get 不会被这条测试调到）
	cache := &configFleetCache{
		// cli 这里留 nil；Get 我们不测
		cli:           nil,
		configBaseURL: srv.URL,
		httpc:         srv.Client(),
		namespace:     "accounting-system",
		actor:         "test-actor",
		logger:        zap.NewNop(),
	}

	// 构造 100 个 sub-account（其中 5 个空，模拟部分 provision）
	subs := make([]string, 100)
	for i := 0; i < 100; i++ {
		if i%20 == 0 {
			continue // 5 个空
		}
		subs[i] = "608" + strconv.Itoa(i) + "00011600049"
	}

	err := cache.Push(context.Background(), 42, subs)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/api/v1/configs/accounting-system/fleet.42", gotURLPath)
	assert.Equal(t, "test-actor", gotBody["actor"])
	assert.Contains(t, gotBody["change_reason"], "la_id=42")
	assert.Equal(t, "json", gotBody["format"])
	// 非空 sub 应该被编码到 map；空 sub 不在 map 里（避免 "" 误命中）
	assert.Equal(t, 95, len(gotJSONMap), "5 empty sub-accounts should be skipped")
	for i, want := range subs {
		got := gotJSONMap[strconv.Itoa(i)]
		if want == "" {
			assert.Equal(t, "", got, "sub_idx %d should be absent", i)
		} else {
			assert.Equal(t, want, got, "sub_idx %d should match", i)
		}
	}
}

// Push 时长度不对应该报错（防 caller 误传）。
func TestFleetCache_PushWrongLen(t *testing.T) {
	cache := &configFleetCache{
		configBaseURL: "http://example.com",
		httpc:         &http.Client{},
		namespace:     "accounting-system",
		actor:         "x",
		logger:        zap.NewNop(),
	}
	err := cache.Push(context.Background(), 1, make([]string, 50)) // 50 != 100
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expected 100")
}

// Push 时 server 返非 2xx，应返回 error 给 caller。
func TestFleetCache_PushServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`upstream unreachable`))
	}))
	defer srv.Close()

	cache := &configFleetCache{
		configBaseURL: srv.URL,
		httpc:         srv.Client(),
		namespace:     "accounting-system",
		actor:         "x",
		logger:        zap.NewNop(),
	}
	err := cache.Push(context.Background(), 99, make([]string, 100))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "502")
}

// fleetKey 命名稳定性：保证以后兼容性，key 不能改变命名。
func TestFleetKey(t *testing.T) {
	assert.Equal(t, "fleet.0", fleetKey(0))
	assert.Equal(t, "fleet.42", fleetKey(42))
	assert.Equal(t, "fleet.9999999", fleetKey(9999999))
}
