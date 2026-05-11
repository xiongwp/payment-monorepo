// 01_merchant_demo.go — 商户调用 demo: 用 oauth2client SDK 拿 token 调 API。
//
// 演示:
//   1. SDK 自动拿 token (client_credentials)
//   2. 第 1 次调用 — fetch token + 调 API
//   3. 第 2 次调用 — 走 token 缓存 (无 fetch)
//   4. 故意请求超出 scope → 403
//
// 运行:
//   source .env
//   go run ./01_merchant_demo.go

//go:build ignore

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"reconcile-system/packages/oauth2-server/client"
)

func main() {
	oauthHost := mustEnv("OAUTH_HOST")
	cid := mustEnv("MER_CLIENT_ID")
	csec := mustEnv("MER_CLIENT_SECRET")
	apiBase := envOr("API_BASE", "http://localhost:9090")

	fmt.Printf("\n=========================================\n")
	fmt.Printf("  商户 OAuth2 集成 Demo\n")
	fmt.Printf("  client_id = %s\n", cid)
	fmt.Printf("  oauth_host = %s\n", oauthHost)
	fmt.Printf("  api_base = %s\n", apiBase)
	fmt.Printf("=========================================\n")

	// ── Step 1: 初始化 SDK (拿到 client_id + secret 就够) ──
	tc := client.New(client.Config{
		TokenURL:     oauthHost + "/oauth2/token",
		ClientID:     cid,
		ClientSecret: csec,
		Scope:        "charge:write refund:write",
	})

	// ── Step 2: 第 1 次调用 — fetch token + 调 API ──
	fmt.Printf("\n▶ [1] 创建 charge (charge:write scope)\n")
	resp := must(call(tc, "POST", apiBase+"/api/v1/charges", map[string]any{
		"amount_minor": 100,
		"currency":     "USD",
		"description":  "demo charge",
	}))
	dump(resp)

	// ── Step 3: 第 2 次调用 — token 已缓存，不再 fetch ──
	fmt.Printf("\n▶ [2] 列 charges (charge:read — 注: 我们 scope 没要 read，应被拒)\n")
	resp = must(call(tc, "GET", apiBase+"/api/v1/charges", nil))
	dump(resp)
	if resp.StatusCode == 403 {
		fmt.Printf("  ✓ 符合预期: scope 不包含 charge:read → 403\n")
	}

	// ── Step 4: 看身份 (whoami) ──
	fmt.Printf("\n▶ [3] 查身份 — /api/v1/whoami\n")
	resp = must(call(tc, "GET", apiBase+"/api/v1/whoami", nil))
	dump(resp)

	// ── Step 5: 创建退款 ──
	fmt.Printf("\n▶ [4] 创建 refund (refund:write scope)\n")
	resp = must(call(tc, "POST", apiBase+"/api/v1/refunds", map[string]any{
		"charge_id":    "ch_001",
		"amount_minor": 50,
	}))
	dump(resp)

	// ── Step 6: 拿原始 token 看一眼 ──
	tok, err := tc.Token(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n▶ [5] 当前 access_token (头 40 字符):\n  %s...\n", tok[:40])
	fmt.Printf("    完整 JWT 在 jwt.io 可解码看 claims (header.payload.sig)\n")

	fmt.Printf("\n✅ 商户 demo 完成\n")
}

// ─── helpers ───────────────────────────────────────────────────────────

func call(tc *client.TokenClient, method, url string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return tc.Do(req)
}

func dump(resp *http.Response) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("  HTTP %d\n", resp.StatusCode)
	// pretty print JSON
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "  ", "  ") == nil {
		fmt.Printf("  %s\n", pretty.String())
	} else {
		fmt.Printf("  %s\n", string(body))
	}
}

func must(r *http.Response, err error) *http.Response {
	if err != nil {
		log.Fatalf("request failed: %v", err)
	}
	return r
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("env %s required (did you `source .env`?)", k)
	}
	return v
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
