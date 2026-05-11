// 02_service_demo.go — 服务间调用 demo (payment-gateway 调 refund-engine 等)。
//
// 跟商户 demo 区别:
//   - owner_type = service (token claim 里 owner_id = 服务名)
//   - resource server 端 actor.Type = "service", actor.ServiceID = "payment-gateway"
//   - 通常 service-to-service 直接调，不走商户隔离
//
// 运行:
//   source .env && go run ./02_service_demo.go

//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"reconcile-system/packages/oauth2-server/client"
)

func main() {
	oauthHost := mustEnv("OAUTH_HOST")
	cid := mustEnv("SVC_CLIENT_ID")
	csec := mustEnv("SVC_CLIENT_SECRET")
	apiBase := envOr("API_BASE", "http://localhost:9090")

	fmt.Printf("\n=========================================\n")
	fmt.Printf("  服务间调用 OAuth2 Demo\n")
	fmt.Printf("  caller = payment-gateway\n")
	fmt.Printf("  callee = demo-resource-server\n")
	fmt.Printf("=========================================\n")

	tc := client.New(client.Config{
		TokenURL:     oauthHost + "/oauth2/token",
		ClientID:     cid,
		ClientSecret: csec,
		Scope:        "refund:write refund:read",
	})

	// 模拟 payment-gateway 处理订单 → 触发退款 → 调 refund-engine
	fmt.Printf("\n▶ [1] payment-gateway 收到商户退款请求\n")
	orderID := fmt.Sprintf("order_%d", time.Now().UnixNano())

	fmt.Printf("\n▶ [2] payment-gateway 调 refund-engine (Bearer JWT 自动加)\n")
	resp, err := callJSON(tc, "POST", apiBase+"/api/v1/refunds", map[string]any{
		"charge_id":    "ch_for_" + orderID,
		"amount_minor": 199,
	})
	if err != nil {
		log.Fatal(err)
	}
	dump(resp)

	fmt.Printf("\n▶ [3] 验证 actor.Type=service (而不是 merchant)\n")
	resp, _ = callJSON(tc, "GET", apiBase+"/api/v1/whoami", nil)
	dump(resp)

	// 并发场景 — 多 goroutine 共享一个 TokenClient (validate caching/mutex 正确)
	fmt.Printf("\n▶ [4] 并发 10 个请求 (验 SDK 锁/缓存)\n")
	done := make(chan int, 10)
	start := time.Now()
	for i := 0; i < 10; i++ {
		go func(i int) {
			resp, err := callJSON(tc, "GET", apiBase+"/api/v1/refunds", nil)
			if err != nil {
				done <- 0
				return
			}
			resp.Body.Close()
			done <- resp.StatusCode
		}(i)
	}
	ok := 0
	for i := 0; i < 10; i++ {
		if c := <-done; c == 200 {
			ok++
		}
	}
	fmt.Printf("  10 并发: %d 成功, 耗时 %s\n", ok, time.Since(start))

	fmt.Printf("\n✅ 服务间 demo 完成 — 关键点: 不需要每个请求重新拿 token (SDK 自动缓存)\n")
}

// ─── helpers ───────────────────────────────────────────────────────────

func callJSON(tc *client.TokenClient, method, url string, body any) (*http.Response, error) {
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
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "  ", "  ") == nil {
		fmt.Printf("  %s\n", pretty.String())
	} else {
		fmt.Printf("  %s\n", string(body))
	}
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
