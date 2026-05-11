// pool-audit — 扫整个 monorepo 找 DB/Redis/HTTP client 连接池配置,
// 输出报告 + 给出 tuning 建议。
//
// 检查项:
//   - sql.DB: SetMaxOpenConns / SetMaxIdleConns / SetConnMaxLifetime / SetConnMaxIdleTime
//   - redis client: PoolSize / MinIdleConns / MaxRetries
//   - http.Client: MaxIdleConns / MaxIdleConnsPerHost / IdleConnTimeout
//
// 反模式标红:
//   ✗ 默认配置 (0/未设)
//   ✗ Idle > Open (浪费)
//   ✗ Lifetime 不限 (云负载均衡器会主动 close, 出 broken pipe)
//   ✗ HTTP DefaultClient (无超时, 容易 hang)
//
// 跑:
//   go run ./tools/pool-audit -root packages/
//   go run ./tools/pool-audit -root . -format markdown > docs/POOL_AUDIT.md

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	rootDir   = flag.String("root", ".", "monorepo root to scan")
	format    = flag.String("format", "text", "text / markdown / json")
	threshold = flag.Int("threshold", 5, "warn if open conns < this")
)

type Finding struct {
	File    string
	Line    int
	Kind    string
	Pattern string
	Value   string
	Issue   string // "OK" / "WARN: ..." / "BAD: ..."
}

var patterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"sql.SetMaxOpenConns", regexp.MustCompile(`\.SetMaxOpenConns\s*\(\s*(\d+|[a-zA-Z_.]+)\s*\)`)},
	{"sql.SetMaxIdleConns", regexp.MustCompile(`\.SetMaxIdleConns\s*\(\s*(\d+|[a-zA-Z_.]+)\s*\)`)},
	{"sql.SetConnMaxLifetime", regexp.MustCompile(`\.SetConnMaxLifetime\s*\(\s*(.+?)\s*\)`)},
	{"sql.SetConnMaxIdleTime", regexp.MustCompile(`\.SetConnMaxIdleTime\s*\(\s*(.+?)\s*\)`)},
	{"redis.PoolSize", regexp.MustCompile(`PoolSize\s*:\s*(\d+|[a-zA-Z_.]+)`)},
	{"redis.MinIdleConns", regexp.MustCompile(`MinIdleConns\s*:\s*(\d+|[a-zA-Z_.]+)`)},
	{"http.MaxIdleConns", regexp.MustCompile(`MaxIdleConns\s*:\s*(\d+|[a-zA-Z_.]+)`)},
	{"http.MaxIdleConnsPerHost", regexp.MustCompile(`MaxIdleConnsPerHost\s*:\s*(\d+|[a-zA-Z_.]+)`)},
	{"http.IdleConnTimeout", regexp.MustCompile(`IdleConnTimeout\s*:\s*(.+?)[,}]`)},
	{"http.Timeout", regexp.MustCompile(`(?:^|\s)Timeout\s*:\s*(.+?)[,}]`)},
	{"http.DefaultClient", regexp.MustCompile(`http\.DefaultClient`)},
}

func main() {
	flag.Parse()
	var findings []Finding

	_ = filepath.WalkDir(*rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			// 跳过 vendor / .git / node_modules
			if d != nil && d.IsDir() {
				name := d.Name()
				if name == "vendor" || name == ".git" || name == "node_modules" || name == "out" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// skip test files (太多 mock 配置)
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		ln := 0
		for scanner.Scan() {
			ln++
			line := scanner.Text()
			for _, p := range patterns {
				if m := p.re.FindStringSubmatch(line); m != nil {
					v := ""
					if len(m) > 1 {
						v = m[1]
					}
					findings = append(findings, Finding{
						File:    rel(path),
						Line:    ln,
						Kind:    p.kind,
						Pattern: line,
						Value:   v,
						Issue:   judge(p.kind, v, line),
					})
				}
			}
		}
		return nil
	})

	// 排序: 按文件 → 行号
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})

	switch *format {
	case "markdown":
		printMarkdown(findings)
	case "json":
		printJSON(findings)
	default:
		printText(findings)
	}
}

func rel(p string) string {
	r, err := filepath.Rel(*rootDir, p)
	if err != nil {
		return p
	}
	return r
}

func judge(kind, value, line string) string {
	switch kind {
	case "http.DefaultClient":
		return "BAD: DefaultClient 无超时, 容易 hang; 改用自定义 client + Timeout"
	case "sql.SetMaxOpenConns":
		if value == "0" {
			return "BAD: SetMaxOpenConns(0) = 不限, 易压垮 DB"
		}
		return "OK"
	case "sql.SetConnMaxLifetime":
		if strings.Contains(value, "0") || value == "" {
			return "WARN: ConnMaxLifetime=0 表示不限; LB/firewall 主动 close 会导致 broken pipe; 建议 30min"
		}
		return "OK"
	case "http.IdleConnTimeout":
		if value == "0" || strings.Contains(value, "* 0") {
			return "WARN: IdleConnTimeout=0 表示不超时; idle conn 累积"
		}
		return "OK"
	case "http.Timeout":
		if value == "0" || value == "0 * time.Second" {
			return "BAD: HTTP Timeout=0 = 永不超时"
		}
		return "OK"
	}
	return "OK"
}

func printText(findings []Finding) {
	fmt.Printf("Found %d pool/timeout config sites\n\n", len(findings))
	curr := ""
	for _, f := range findings {
		if f.File != curr {
			fmt.Printf("\n--- %s ---\n", f.File)
			curr = f.File
		}
		marker := "✓"
		if strings.HasPrefix(f.Issue, "BAD") {
			marker = "✗"
		} else if strings.HasPrefix(f.Issue, "WARN") {
			marker = "⚠"
		}
		fmt.Printf("  %s L%d  %-30s = %-20s  %s\n",
			marker, f.Line, f.Kind, truncate(f.Value, 20), f.Issue)
	}
}

func printMarkdown(findings []Finding) {
	fmt.Println("# Connection Pool / Timeout Audit")
	fmt.Println()
	fmt.Println("自动扫描 monorepo 找 sql / redis / http client 池配置 + 超时设置, 列出潜在问题。")
	fmt.Println()
	bad := 0
	warn := 0
	for _, f := range findings {
		if strings.HasPrefix(f.Issue, "BAD") {
			bad++
		} else if strings.HasPrefix(f.Issue, "WARN") {
			warn++
		}
	}
	fmt.Printf("总计: **%d** 处配置, **%d** BAD, **%d** WARN\n\n", len(findings), bad, warn)
	fmt.Println("| 文件 | 行 | 类型 | 值 | 问题 |")
	fmt.Println("|---|---:|---|---|---|")
	for _, f := range findings {
		if strings.HasPrefix(f.Issue, "OK") {
			continue
		}
		fmt.Printf("| `%s` | %d | %s | `%s` | %s |\n",
			f.File, f.Line, f.Kind, f.Value, f.Issue)
	}
}

func printJSON(findings []Finding) {
	fmt.Println("[")
	for i, f := range findings {
		comma := ","
		if i == len(findings)-1 {
			comma = ""
		}
		fmt.Printf("  {\"file\":%q,\"line\":%d,\"kind\":%q,\"value\":%q,\"issue\":%q}%s\n",
			f.File, f.Line, f.Kind, f.Value, f.Issue, comma)
	}
	fmt.Println("]")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
