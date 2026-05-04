// Command redis-rebuild — disaster-recovery CLI for hot-account Redis state.
//
// 用法（HTTP 模式，本机直连 accounting-system 的 admin HTTP）：
//
//	go run ./cmd/tools/redis-rebuild \
//	    -addr  http://accounting-system:8888 \
//	    -token "$ADMIN_TOKEN"                \
//	    -as-of 5m                            \
//	    -accounts 010100001-001,010100002-002 \
//	    -dry-run
//
// 选项：
//
//	-addr        accounting-system 的 admin HTTP 基址（含 scheme，必填）
//	-token       X-Admin-Token，若服务端开了鉴权则必填
//	-as-of       回放时间点：相对时长（5m / 1h）或 RFC3339 时间戳；空 = 用 account
//	             表当前余额（适合 Redis 重启后立即恢复）
//	-accounts    指定账户号（逗号分隔），缺省走 hot_account_config 全量启用项
//	-dry-run     只算 diff 不写 Redis（输出 BalanceBefore / BalanceAfter）
//	-timeout     单次 HTTP 调用超时，默认 5min（重建 1k+ 账户也够用）
//
// 退出码：
//
//	0  成功
//	1  CLI 参数 / IO 错误
//	2  服务端返回 4xx/5xx
//	3  报告里有 failed > 0
//
// 这条 CLI 通过调 admin HTTP 间接复用 service.RebuildHotAccounts，避免在工具里
// 维护一份 DB / Redis 客户端的副本。也意味着不依赖 accounting-system 内部包，
// 保持 cmd/tools/ 和 internal/ 解耦。同样的逻辑在 admin-web 页面 (POST
// /v1/redis/rebuild → /admin/redis/rebuild) 下也能触发，CLI 是 admin-web 不可
// 用时（比如 admin-web 自己挂了）的兜底操作通道。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	var (
		addr     = flag.String("addr", "", "accounting-system admin HTTP base URL (required)")
		token    = flag.String("token", "", "X-Admin-Token header value")
		asOf     = flag.String("as-of", "", `time cutoff: duration (e.g. "5m") or RFC3339 timestamp; empty = use current MySQL balance`)
		accounts = flag.String("accounts", "", "comma-separated account_no list (default: all enabled hot accounts)")
		dryRun   = flag.Bool("dry-run", false, "do not write Redis; just print the diff report")
		timeout  = flag.Duration("timeout", 5*time.Minute, "HTTP call timeout")
	)
	flag.Parse()

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "redis-rebuild: -addr is required, e.g. http://accounting-system:8888")
		os.Exit(1)
	}

	body := map[string]interface{}{
		"dry_run": *dryRun,
	}
	if *asOf != "" {
		body["as_of"] = *asOf
	}
	if *accounts != "" {
		var list []string
		for _, a := range strings.Split(*accounts, ",") {
			if a = strings.TrimSpace(a); a != "" {
				list = append(list, a)
			}
		}
		if len(list) > 0 {
			body["account_nos"] = list
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal body:", err)
		os.Exit(1)
	}

	url := strings.TrimRight(*addr, "/") + "/admin/redis/rebuild"
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintln(os.Stderr, "build request:", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	if *token != "" {
		req.Header.Set("X-Admin-Token", *token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "http call:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(os.Stderr, "server returned %d: %s\n", resp.StatusCode, string(respBody))
		os.Exit(2)
	}

	var report struct {
		AsOf      string `json:"as_of"`
		DryRun    bool   `json:"dry_run"`
		Total     int    `json:"total"`
		Updated   int    `json:"updated"`
		Skipped   int    `json:"skipped"`
		Failed    int    `json:"failed"`
		StartedAt string `json:"started_at"`
		Duration  string `json:"duration"`
		Entries   []struct {
			AccountNo     string `json:"account_no"`
			BalanceBefore string `json:"balance_before"`
			BalanceAfter  string `json:"balance_after"`
			Source        string `json:"source"`
			JournalCutoff string `json:"journal_cutoff,omitempty"`
			Skipped       bool   `json:"skipped"`
			Reason        string `json:"reason,omitempty"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(respBody, &report); err != nil {
		fmt.Fprintln(os.Stderr, "decode report:", err, "; raw:", string(respBody))
		os.Exit(1)
	}

	fmt.Printf("Redis rebuild report\n")
	fmt.Printf("  as_of:      %s\n", report.AsOf)
	fmt.Printf("  dry_run:    %v\n", report.DryRun)
	fmt.Printf("  total:      %d\n", report.Total)
	fmt.Printf("  updated:    %d\n", report.Updated)
	fmt.Printf("  skipped:    %d\n", report.Skipped)
	fmt.Printf("  failed:     %d\n", report.Failed)
	fmt.Printf("  duration:   %s\n", report.Duration)
	fmt.Printf("\nPer-account:\n")
	for _, e := range report.Entries {
		flag := "OK "
		switch {
		case e.Reason != "" && !e.Skipped:
			flag = "ERR"
		case e.Skipped:
			flag = "SKP"
		}
		line := fmt.Sprintf("  [%s] %-30s before=%-20s after=%-20s source=%s",
			flag, e.AccountNo, valOrDash(e.BalanceBefore), valOrDash(e.BalanceAfter), e.Source)
		if e.JournalCutoff != "" {
			line += " cutoff=" + e.JournalCutoff
		}
		if e.Reason != "" {
			line += "  -- " + e.Reason
		}
		fmt.Println(line)
	}

	if report.Failed > 0 {
		os.Exit(3)
	}
}

func valOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
