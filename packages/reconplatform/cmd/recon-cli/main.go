// Command recon-cli — 本地试跑 Starlark 规则、import/export catalog、生成 fixture.
//
// 子命令:
//
//	recon-cli rule test     --script=foo.star --fixture=foo.yaml [--params k=v]
//	recon-cli rule lint     --catalog=internal/catalog/scripts
//	recon-cli rule test-all --catalog=internal/catalog/scripts --fixtures=internal/catalog/scripts/fixtures
//	recon-cli catalog export --admin=URL --out=catalog.yaml
//	recon-cli catalog import --admin=URL --in=catalog.yaml
//	recon-cli catalog diff   --admin=URL --local=internal/catalog/scripts
//	recon-cli fixture gen    --rule=NAME --hours=N --admin=URL --out=fixture.yaml
//
// 设计:
//   - 全离线模式跑规则:走 FixtureSearcher,不依赖 Redis / Kafka
//   - import/export 用 admin HTTP API (/api/v1/scripts) 走真实 API,
//     不依赖任何客户端 SDK
//   - 输出彩色 + 退出码: 0 ok, 1 diffs / lint error, 2 IO error
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorDim    = "\033[2m"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "rule":
		ruleCmd(os.Args[2:])
	case "catalog":
		catalogCmd(os.Args[2:])
	case "fixture":
		fixtureCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("recon-cli v0.1.0")
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `recon-cli — reconplatform 本地开发工具.

USAGE:
  recon-cli rule test     --script=FILE --fixture=FILE [--params k=v,...]
  recon-cli rule lint     --catalog=DIR
  recon-cli rule test-all --catalog=DIR --fixtures=DIR
  recon-cli catalog export --admin=URL --out=FILE
  recon-cli catalog import --admin=URL --in=FILE
  recon-cli catalog diff   --admin=URL --local=DIR
  recon-cli fixture gen    --rule=NAME --hours=N --admin=URL --out=FILE

EXAMPLES:
  recon-cli rule test \
      --script=internal/catalog/scripts/duplicate_charge.star \
      --fixture=internal/catalog/scripts/fixtures/duplicate_charge_happy.yaml

  recon-cli catalog export --admin=http://localhost:8080 --out=prod-rules.yaml`)
}

// ─── rule ─────────────────────────────────────────────────────

func ruleCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "rule: missing sub-command (test/lint/test-all)")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "test":
		ruleTest(rest)
	case "lint":
		ruleLint(rest)
	case "test-all":
		ruleTestAll(rest)
	default:
		fmt.Fprintf(os.Stderr, "rule: unknown sub %s\n", sub)
		os.Exit(2)
	}
}

type ruleTestFlags struct {
	scriptPath  string
	fixturePath string
	paramsStr   string
	maxSteps    int64
	timeout     time.Duration
	verbose     bool
}

func ruleTest(args []string) {
	fs := flag.NewFlagSet("rule test", flag.ExitOnError)
	f := ruleTestFlags{}
	fs.StringVar(&f.scriptPath, "script", "", "Starlark script (.star) path")
	fs.StringVar(&f.fixturePath, "fixture", "", "Fixture YAML path (events list)")
	fs.StringVar(&f.paramsStr, "params", "", "comma-separated k=v for ctx.params")
	fs.Int64Var(&f.maxSteps, "max-steps", 5_000_000, "Starlark max steps")
	fs.DurationVar(&f.timeout, "timeout", 30*time.Second, "execution deadline")
	fs.BoolVar(&f.verbose, "verbose", false, "print fixture summary + step counts")
	_ = fs.Parse(args)
	if f.scriptPath == "" || f.fixturePath == "" {
		fs.Usage()
		os.Exit(2)
	}

	code, err := os.ReadFile(f.scriptPath)
	if err != nil {
		fail("read script: %v", err)
	}
	fixture, err := loadFixture(f.fixturePath)
	if err != nil {
		fail("load fixture: %v", err)
	}
	if f.verbose {
		fmt.Printf("%sfixture:%s %d events from %s\n", colorDim, colorReset, len(fixture.Events), f.fixturePath)
	}

	diffs, runErr := runRuleOnFixture(code, filepath.Base(f.scriptPath), fixture, f, f.verbose)
	if runErr != nil {
		fmt.Printf("%s✗ runtime error:%s %v\n", colorRed, colorReset, runErr)
		os.Exit(1)
	}

	expected := fixture.ExpectDiffs
	if expected != nil {
		ok := compareDiffs(diffs, expected)
		if !ok {
			fmt.Printf("%s✗ FAIL%s — expected %d diffs, got %d\n",
				colorRed, colorReset, len(expected), len(diffs))
			printDiffs("expected", expected)
			printDiffs("actual  ", diffs)
			os.Exit(1)
		}
		fmt.Printf("%s✓ PASS%s — %d diffs match\n", colorGreen, colorReset, len(diffs))
		return
	}

	// 没声明 expect 就只打印
	if len(diffs) == 0 {
		fmt.Printf("%s✓ no diffs%s\n", colorGreen, colorReset)
	} else {
		fmt.Printf("%s● %d diff(s):%s\n", colorYellow, len(diffs), colorReset)
		printDiffs("", diffs)
	}
}

func ruleLint(args []string) {
	fs := flag.NewFlagSet("rule lint", flag.ExitOnError)
	dir := fs.String("catalog", "internal/catalog/scripts", "catalog dir")
	_ = fs.Parse(args)

	files, err := filepath.Glob(filepath.Join(*dir, "*.star"))
	if err != nil {
		fail("glob: %v", err)
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no .star files in %s\n", *dir)
		os.Exit(2)
	}
	engine := script.NewEngine(5_000_000)
	bad := 0
	for _, p := range files {
		code, err := os.ReadFile(p)
		if err != nil {
			fmt.Printf("%s✗%s %s — read: %v\n", colorRed, colorReset, p, err)
			bad++
			continue
		}
		_, err = engine.Compile(filepath.Base(p), string(code))
		if err != nil {
			fmt.Printf("%s✗%s %s\n  %s\n", colorRed, colorReset, p, err)
			bad++
			continue
		}
		fmt.Printf("%s✓%s %s\n", colorGreen, colorReset, p)
	}
	if bad > 0 {
		fmt.Printf("\n%s%d/%d failed%s\n", colorRed, bad, len(files), colorReset)
		os.Exit(1)
	}
	fmt.Printf("\n%s✓ all %d rules compile%s\n", colorGreen, len(files), colorReset)
}

func ruleTestAll(args []string) {
	fs := flag.NewFlagSet("rule test-all", flag.ExitOnError)
	catalogDir := fs.String("catalog", "internal/catalog/scripts", "catalog dir")
	fixturesDir := fs.String("fixtures", "internal/catalog/scripts/fixtures", "fixtures dir")
	_ = fs.Parse(args)

	rules, err := filepath.Glob(filepath.Join(*catalogDir, "*.star"))
	if err != nil {
		fail("glob: %v", err)
	}
	if len(rules) == 0 {
		fmt.Fprintf(os.Stderr, "no .star rules in %s\n", *catalogDir)
		os.Exit(2)
	}
	var (
		pass, fail, skip int
	)
	for _, r := range rules {
		name := strings.TrimSuffix(filepath.Base(r), ".star")
		fixtures, _ := filepath.Glob(filepath.Join(*fixturesDir, name+"_*.yaml"))
		if len(fixtures) == 0 {
			fmt.Printf("%s~%s %s (no fixture, skip)\n", colorDim, colorReset, name)
			skip++
			continue
		}
		code, err := os.ReadFile(r)
		if err != nil {
			fmt.Printf("%s✗%s %s read: %v\n", colorRed, colorReset, name, err)
			fail++
			continue
		}
		ruleFail := false
		for _, fx := range fixtures {
			fixture, err := loadFixture(fx)
			if err != nil {
				fmt.Printf("%s✗%s %s [%s] load: %v\n", colorRed, colorReset, name, filepath.Base(fx), err)
				ruleFail = true
				continue
			}
			rt := ruleTestFlags{scriptPath: r, maxSteps: 5_000_000, timeout: 30 * time.Second}
			diffs, runErr := runRuleOnFixture(code, name, fixture, rt, false)
			if runErr != nil {
				fmt.Printf("%s✗%s %s [%s] runtime: %v\n", colorRed, colorReset, name, filepath.Base(fx), runErr)
				ruleFail = true
				continue
			}
			if fixture.ExpectDiffs != nil && !compareDiffs(diffs, fixture.ExpectDiffs) {
				fmt.Printf("%s✗%s %s [%s] diff mismatch (want %d got %d)\n",
					colorRed, colorReset, name, filepath.Base(fx),
					len(fixture.ExpectDiffs), len(diffs))
				ruleFail = true
				continue
			}
			fmt.Printf("%s✓%s %s [%s] (%d diff)\n", colorGreen, colorReset, name, filepath.Base(fx), len(diffs))
		}
		if ruleFail {
			fail++
		} else {
			pass++
		}
	}
	fmt.Printf("\n%spass=%d fail=%d skip=%d%s\n", colorBlue, pass, fail, skip, colorReset)
	if fail > 0 {
		os.Exit(1)
	}
}

// ─── catalog ──────────────────────────────────────────────────

type catalogEntry struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Severity    string `yaml:"severity,omitempty"`
	Schedule    string `yaml:"schedule,omitempty"`
	Code        string `yaml:"code"`
}

type catalogFile struct {
	GeneratedAt time.Time      `yaml:"generated_at"`
	Source      string         `yaml:"source,omitempty"`
	Entries     []catalogEntry `yaml:"entries"`
}

func catalogCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "catalog: missing sub-command")
		os.Exit(2)
	}
	switch args[0] {
	case "export":
		catalogExport(args[1:])
	case "import":
		catalogImport(args[1:])
	case "diff":
		catalogDiff(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "catalog: unknown sub %s\n", args[0])
		os.Exit(2)
	}
}

func catalogExport(args []string) {
	fs := flag.NewFlagSet("catalog export", flag.ExitOnError)
	admin := fs.String("admin", "http://localhost:8080", "admin web URL")
	out := fs.String("out", "catalog.yaml", "output file")
	_ = fs.Parse(args)

	list, err := httpGetJSON(*admin + "/api/v1/scripts")
	if err != nil {
		fail("export: %v", err)
	}
	rows, _ := list.([]interface{})
	cf := catalogFile{GeneratedAt: time.Now(), Source: *admin}
	for _, r := range rows {
		m, _ := r.(map[string]interface{})
		id, _ := m["id"].(string)
		full, err := httpGetJSON(*admin + "/api/v1/scripts/" + id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", id, err)
			continue
		}
		fm, _ := full.(map[string]interface{})
		cf.Entries = append(cf.Entries, catalogEntry{
			ID:          asStr(fm["id"]),
			Name:        asStr(fm["name"]),
			Description: asStr(fm["description"]),
			Severity:    asStr(fm["severity"]),
			Schedule:    asStr(fm["schedule"]),
			Code:        asStr(fm["code"]),
		})
	}
	data, err := yaml.Marshal(cf)
	if err != nil {
		fail("yaml: %v", err)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fail("write: %v", err)
	}
	fmt.Printf("%s✓%s exported %d entries → %s\n", colorGreen, colorReset, len(cf.Entries), *out)
}

func catalogImport(args []string) {
	fs := flag.NewFlagSet("catalog import", flag.ExitOnError)
	admin := fs.String("admin", "http://localhost:8080", "admin web URL")
	in := fs.String("in", "", "input YAML")
	dryRun := fs.Bool("dry-run", false, "validate only, no POST")
	_ = fs.Parse(args)
	if *in == "" {
		fs.Usage()
		os.Exit(2)
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		fail("read: %v", err)
	}
	var cf catalogFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		fail("yaml: %v", err)
	}
	if *dryRun {
		fmt.Printf("(dry-run) would import %d entries to %s\n", len(cf.Entries), *admin)
		return
	}
	ok, total := 0, len(cf.Entries)
	for _, e := range cf.Entries {
		body := map[string]any{
			"name":        e.Name,
			"description": e.Description,
			"severity":    e.Severity,
			"schedule":    e.Schedule,
			"code":        e.Code,
		}
		// 先尝试 PUT (按 id) 失败再 POST 创建
		err := httpPutJSON(*admin+"/api/v1/scripts/"+e.ID, body)
		if err != nil {
			if _, perr := httpPostJSON(*admin+"/api/v1/scripts", body); perr != nil {
				fmt.Printf("%s✗%s %s: %v\n", colorRed, colorReset, e.ID, perr)
				continue
			}
		}
		fmt.Printf("%s✓%s %s\n", colorGreen, colorReset, e.ID)
		ok++
	}
	fmt.Printf("\n%s%d/%d imported%s\n", colorBlue, ok, total, colorReset)
	if ok < total {
		os.Exit(1)
	}
}

func catalogDiff(args []string) {
	fs := flag.NewFlagSet("catalog diff", flag.ExitOnError)
	admin := fs.String("admin", "http://localhost:8080", "admin web URL")
	local := fs.String("local", "internal/catalog/scripts", "local catalog dir")
	_ = fs.Parse(args)

	// 拉远端
	list, err := httpGetJSON(*admin + "/api/v1/scripts")
	if err != nil {
		fail("diff fetch: %v", err)
	}
	remoteSet := map[string]bool{}
	rows, _ := list.([]interface{})
	for _, r := range rows {
		m, _ := r.(map[string]interface{})
		if id, ok := m["id"].(string); ok {
			remoteSet[id] = true
		}
	}
	// 扫本地
	files, _ := filepath.Glob(filepath.Join(*local, "*.star"))
	localSet := map[string]bool{}
	for _, p := range files {
		name := strings.TrimSuffix(filepath.Base(p), ".star")
		localSet[name] = true
	}
	// 三向: only-local / only-remote / both
	var onlyLocal, onlyRemote, both []string
	for k := range localSet {
		if remoteSet[k] {
			both = append(both, k)
		} else {
			onlyLocal = append(onlyLocal, k)
		}
	}
	for k := range remoteSet {
		if !localSet[k] {
			onlyRemote = append(onlyRemote, k)
		}
	}
	sort.Strings(onlyLocal)
	sort.Strings(onlyRemote)
	sort.Strings(both)

	fmt.Printf("%sonly local (%d):%s\n", colorYellow, len(onlyLocal), colorReset)
	for _, k := range onlyLocal {
		fmt.Println("  -", k)
	}
	fmt.Printf("\n%sonly remote (%d):%s\n", colorYellow, len(onlyRemote), colorReset)
	for _, k := range onlyRemote {
		fmt.Println("  -", k)
	}
	fmt.Printf("\n%sshared (%d)%s\n", colorBlue, len(both), colorReset)
}

// ─── fixture ──────────────────────────────────────────────────

func fixtureCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fixture: missing sub-command (gen)")
		os.Exit(2)
	}
	switch args[0] {
	case "gen":
		fixtureGen(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "fixture: unknown sub %s\n", args[0])
		os.Exit(2)
	}
}

func fixtureGen(args []string) {
	fs := flag.NewFlagSet("fixture gen", flag.ExitOnError)
	rule := fs.String("rule", "", "rule name")
	hours := fs.Int("hours", 24, "lookback hours")
	admin := fs.String("admin", "http://localhost:8080", "admin web URL")
	out := fs.String("out", "", "output yaml")
	_ = fs.Parse(args)
	if *rule == "" || *out == "" {
		fs.Usage()
		os.Exit(2)
	}
	// 调 admin 的 fixture export 端点 (admin web 已实现 /api/v1/scripts/:id/_replay_dump 或类似;
	// 若未实现,这里返清晰错误指示要后端补)
	url := fmt.Sprintf("%s/api/v1/scripts/%s/_replay_dump?hours=%d", *admin, *rule, *hours)
	resp, err := httpGetJSON(url)
	if err != nil {
		fail("fixture gen: %v (确保 admin web 实现了 /_replay_dump 端点)", err)
	}
	data, _ := yaml.Marshal(resp)
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fail("write: %v", err)
	}
	fmt.Printf("%s✓%s wrote fixture → %s\n", colorGreen, colorReset, *out)
}

// ─── fixture YAML 结构 ────────────────────────────────────────

// fixtureFile 是 fixture YAML 的 schema.
//
//	events: 喂给 FixtureSearcher 的 store.Event 列表
//	expect_diffs: 期望脚本输出的 Diff 列表 (可选,不填则只打印不断言)
//	params: 注入 ctx.Params 的 KV
type fixtureFile struct {
	Events      []store.Event   `yaml:"events"`
	ExpectDiffs []script.Diff   `yaml:"expect_diffs,omitempty"`
	Params      map[string]string `yaml:"params,omitempty"`
}

func loadFixture(path string) (*fixtureFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f fixtureFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}
	return &f, nil
}

// ─── 跑规则 ────────────────────────────────────────────────────

func runRuleOnFixture(code []byte, scriptID string, fx *fixtureFile, opts ruleTestFlags, verbose bool) ([]script.Diff, error) {
	engine := script.NewEngine(opts.maxSteps)
	compiled, err := engine.Compile(scriptID, string(code))
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	fs := store.NewFixtureSearcher(fx.Events)
	params := fx.Params
	if opts.paramsStr != "" {
		params = mergeParams(params, opts.paramsStr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	sctx := script.NewContext(ctx, fs, &consoleLogger{verbose: verbose}, params)
	return engine.Run(ctx, compiled, sctx)
}

func mergeParams(base map[string]string, csv string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for _, kv := range strings.Split(csv, ",") {
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return out
}

// ─── diff 比较 ────────────────────────────────────────────────

// compareDiffs 顺序无关 + JSON 等值比较.
func compareDiffs(got, want []script.Diff) bool {
	if len(got) != len(want) {
		return false
	}
	usedW := make([]bool, len(want))
	for _, g := range got {
		matched := false
		gjson, _ := json.Marshal(g)
		for j, w := range want {
			if usedW[j] {
				continue
			}
			wjson, _ := json.Marshal(w)
			if string(gjson) == string(wjson) {
				usedW[j] = true
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func printDiffs(prefix string, diffs []script.Diff) {
	for i, d := range diffs {
		j, _ := json.MarshalIndent(d, "  ", "  ")
		fmt.Printf("  %s[%d]: %s\n", prefix, i, string(j))
	}
}

// ─── HTTP helpers ─────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 30 * time.Second}

func httpGetJSON(url string) (any, error) {
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return v, nil
}

func httpPutJSON(url string, body any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", url, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s: %d %s", url, resp.StatusCode, string(rb))
	}
	return nil
}

func httpPostJSON(url string, body any) (any, error) {
	b, _ := json.Marshal(body)
	resp, err := httpClient.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s: %d %s", url, resp.StatusCode, string(rb))
	}
	var v any
	_ = json.Unmarshal(rb, &v)
	return v, nil
}

// ─── helpers ──────────────────────────────────────────────────

func asStr(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s✗ %s\n", colorRed, fmt.Sprintf(format, args...))
	fmt.Fprintln(os.Stderr, colorReset)
	os.Exit(2)
}

// ─── logger ───────────────────────────────────────────────────

type consoleLogger struct{ verbose bool }

func (l *consoleLogger) Info(msg string, kv ...any) {
	if !l.verbose {
		return
	}
	fmt.Printf("%s[INFO]%s %s %v\n", colorDim, colorReset, msg, kv)
}
func (l *consoleLogger) Warn(msg string, kv ...any) {
	fmt.Printf("%s[WARN]%s %s %v\n", colorYellow, colorReset, msg, kv)
}
func (l *consoleLogger) Error(msg string, kv ...any) {
	fmt.Printf("%s[ERR]%s %s %v\n", colorRed, colorReset, msg, kv)
}
