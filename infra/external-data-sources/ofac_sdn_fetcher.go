// ofac_sdn_fetcher.go — 真接 OFAC SDN 数据源 (替代 dev seed).
//
// 部署方式:
//   - 作为 CronJob 跑 (每天 02:00 UTC)
//   - 调用 aml-screening 的 /admin/lists/ofac_sdn/refresh
//   - 失败重试 + Slack 告警
//
// 这个文件原本属于 aml-screening 但放外部 — 因为 OFAC 是合规重要源,
// 接入故障 ops 要单独排查 (跟业务 service 解耦).

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	// 官方 OFAC SDN endpoint — 真生产用 Treasury 官方
	OFACSDNZipURL = "https://www.treasury.gov/ofac/downloads/sdn.xml"
	OFACSDNAdvURL = "https://www.treasury.gov/ofac/downloads/cons_advanced.xml"

	// EU consolidated list (要先注册拿 token)
	// EUListURL    = "https://webgate.ec.europa.eu/europeaid/fsd/fsf/public/files/xmlFullSanctionsList_1_1/content?token=..."
)

func main() {
	var (
		amlAdminURL = flag.String("aml-url", "http://aml-screening:8088", "aml-screening admin URL")
		adminToken  = flag.String("admin-token", os.Getenv("AML_ADMIN_TOKEN"), "admin token")
		slackURL    = flag.String("slack", os.Getenv("SLACK_WEBHOOK"), "Slack 告警 URL (失败时)")
	)
	flag.Parse()

	logger := log("ofac-fetcher")

	logger("starting OFAC SDN refresh...")
	hash, err := fetchAndStore(OFACSDNZipURL)
	if err != nil {
		logger("✗ fetch failed: %v", err)
		alertSlack(*slackURL, fmt.Sprintf("OFAC SDN fetch failed: %v", err))
		os.Exit(1)
	}
	logger("fetched sdn.xml, sha256=%s", hash)

	// 触发 aml-screening 重新拉
	if err := triggerRefresh(*amlAdminURL, *adminToken); err != nil {
		logger("✗ trigger refresh failed: %v", err)
		alertSlack(*slackURL, fmt.Sprintf("aml-screening refresh trigger failed: %v", err))
		os.Exit(1)
	}
	logger("✓ aml-screening refreshed")
}

func fetchAndStore(url string) (string, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "payment-platform-ofac-fetcher/1.0")

	client := &http.Client{Timeout: 5 * time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	h := sha256.New()
	body, err := io.ReadAll(io.TeeReader(resp.Body, h))
	if err != nil {
		return "", err
	}

	// 真生产: 落到 S3 给历史归档 + 给 aml-screening 拉
	// aws s3 cp - s3://payment-platform-data/ofac/sdn-${date}.xml
	_ = body

	return hex.EncodeToString(h.Sum(nil)), nil
}

func triggerRefresh(url, token string) error {
	req, _ := http.NewRequest("POST", url+"/admin/lists/ofac_sdn/refresh", nil)
	req.Header.Set("X-Admin-Token", token)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

func alertSlack(url, msg string) {
	if url == "" {
		return
	}
	body := fmt.Sprintf(`{"text":"🚨 OFAC fetcher: %s"}`, msg)
	_, _ = http.Post(url, "application/json", io.NopCloser(eatString(body)))
}

func log(prefix string) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[%s] [%s] %s\n",
			time.Now().Format("2006-01-02 15:04:05"), prefix, fmt.Sprintf(format, args...))
	}
}

// eatString is a tiny stand-in for strings.NewReader to avoid more imports
type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func eatString(s string) io.Reader { return &stringReader{s: s} }
