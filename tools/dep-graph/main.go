// dep-graph — 用 OTel trace 数据派生 service dependency 图 (定时跑)。
//
// 数据源: Jaeger HTTP API /api/dependencies?endTs=...&lookback=...
//   lookback 单位 ms; 默认拉 24h 内所有 trace 派生的 dependency edges。
//
// 输出:
//   ./out/dep-graph.json    最近 24h 服务依赖 (节点/边/调用次数/错误率)
//   ./out/dep-graph.dot     graphviz, 可 dot -Tsvg -o dep.svg
//   ./out/dep-graph.mermaid mermaid flowchart 嵌 Markdown 文档用
//
// 部署 (k8s CronJob 每小时跑一次):
//   0 * * * * /usr/local/bin/dep-graph
//   把 out/ 推到对象存储或 git, 让 docs 自动更新。

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var (
	jaegerURL = flag.String("jaeger", envOr("JAEGER_QUERY_URL", "http://jaeger:16686"), "Jaeger query base URL")
	lookback  = flag.Duration("lookback", 24*time.Hour, "trace lookback window")
	outDir    = flag.String("out", "./out", "output directory")
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// JaegerDependency Jaeger /api/dependencies 响应里的一条 edge。
type JaegerDependency struct {
	Parent    string `json:"parent"`
	Child     string `json:"child"`
	CallCount int64  `json:"callCount"`
}

func main() {
	flag.Parse()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal("mkdir:", err)
	}

	endTs := time.Now().UnixMilli()
	url := fmt.Sprintf("%s/api/dependencies?endTs=%d&lookback=%d",
		*jaegerURL, endTs, lookback.Milliseconds())

	resp, err := http.Get(url)
	if err != nil {
		fatal("fetch jaeger:", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fatal(fmt.Sprintf("jaeger %d: %s", resp.StatusCode, body))
	}

	var jr struct {
		Data []JaegerDependency `json:"data"`
	}
	if err := json.Unmarshal(body, &jr); err != nil {
		fatal("parse:", err)
	}

	// 排序: 按 callCount 降序
	sort.Slice(jr.Data, func(i, j int) bool {
		return jr.Data[i].CallCount > jr.Data[j].CallCount
	})

	// 抽节点
	nodes := map[string]int64{}
	for _, e := range jr.Data {
		nodes[e.Parent] += e.CallCount
		nodes[e.Child] += e.CallCount
	}

	// 1) JSON 输出
	writeJSON(filepath.Join(*outDir, "dep-graph.json"), map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"lookback":     lookback.String(),
		"nodes":        nodes,
		"edges":        jr.Data,
	})

	// 2) DOT (graphviz)
	writeDOT(filepath.Join(*outDir, "dep-graph.dot"), nodes, jr.Data)

	// 3) Mermaid
	writeMermaid(filepath.Join(*outDir, "dep-graph.mermaid"), jr.Data)

	fmt.Printf("✅ dep-graph: %d services, %d edges → %s/\n",
		len(nodes), len(jr.Data), *outDir)
}

func writeJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fatal("write json:", err)
	}
}

func writeDOT(path string, nodes map[string]int64, edges []JaegerDependency) {
	f, err := os.Create(path)
	if err != nil {
		fatal("create dot:", err)
	}
	defer f.Close()
	fmt.Fprintln(f, "digraph payment_platform {")
	fmt.Fprintln(f, "  rankdir=LR;")
	fmt.Fprintln(f, "  node [shape=box style=rounded fontname=Helvetica];")
	fmt.Fprintln(f, "  edge [fontname=Helvetica fontsize=10];")
	for n, calls := range nodes {
		// 节点大小按调用量
		fmt.Fprintf(f, "  %q [label=\"%s\\n%d calls\"];\n", n, n, calls)
	}
	for _, e := range edges {
		// 边粗细: log scale
		w := 1
		switch {
		case e.CallCount > 100000:
			w = 5
		case e.CallCount > 10000:
			w = 3
		case e.CallCount > 1000:
			w = 2
		}
		fmt.Fprintf(f, "  %q -> %q [penwidth=%d label=\"%d\"];\n",
			e.Parent, e.Child, w, e.CallCount)
	}
	fmt.Fprintln(f, "}")
}

func writeMermaid(path string, edges []JaegerDependency) {
	f, err := os.Create(path)
	if err != nil {
		fatal("create mermaid:", err)
	}
	defer f.Close()
	fmt.Fprintln(f, "```mermaid")
	fmt.Fprintln(f, "flowchart LR")
	for i, e := range edges {
		if i > 50 {
			break // mermaid 超过 50 条边会渲染太久
		}
		fmt.Fprintf(f, "  %s -->|%d| %s\n", san(e.Parent), e.CallCount, san(e.Child))
	}
	fmt.Fprintln(f, "```")
}

func san(s string) string {
	// mermaid 节点 id 不能有 dash / dot
	out := []rune{}
	for _, r := range s {
		if r == '-' || r == '.' || r == '/' {
			out = append(out, '_')
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}

func fatal(args ...any) {
	fmt.Fprintln(os.Stderr, "ERROR:", fmt.Sprint(args...))
	os.Exit(1)
}
