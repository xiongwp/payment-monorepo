// linter.go — 轻量 Starlark static analyzer。
//
// 不替代解释器（Engine.Compile 已做语法校验）；这里检查"语法正确但实践上
// 容易踩坑"的模式。设计原则：误报率 < 1%（运营会 ignore 掉所有 lint），
// 关键问题 hard error，性能问题 warn，风格问题 info。
//
// 当前规则集（按严重度）：
//
//	error/E001  ctx 参数命名必须为 'ctx'      (engine 注入命名)
//	error/E002  check 必须返 list[dict]       (return 0/None/dict 都不行)
//
//	warn/W001   嵌套 for 循环 (O(N²) 风险)    "for x in ...: for y in ..."
//	warn/W002   循环里调 http_get / scan_index 类 expensive op
//	warn/W003   while True 没限制（Starlark 不允许，但万一未来改）
//	warn/W004   ctx 形参定义但通体没用（脚本可能写错了）
//
//	info/I001   单文件超过 200 行 (建议拆 helper)
//	info/I002   未用 import (load(...) 后从未用)
//
// 集成：Validate 端点 = parse + lint。lint warnings 不阻塞保存，仅在
// admin web 编辑器侧栏 Marker 显示；error 阻塞保存。
//
// AST walk：用 starlark/syntax.Walk + Visit 接口，不重新发明轮子。

package script

import (
	"fmt"
	"strings"

	"go.starlark.net/syntax"
)

// LintSeverity 严重度。
type LintSeverity string

const (
	SeverityError LintSeverity = "error"
	SeverityWarn  LintSeverity = "warn"
	SeverityInfo  LintSeverity = "info"
)

// LintIssue 一条 lint 问题。
type LintIssue struct {
	Code     string       `json:"code"`     // E001 / W001 / I001
	Severity LintSeverity `json:"severity"` // error / warn / info
	Message  string       `json:"message"`  // 人类可读
	Line     int          `json:"line"`     // 1-indexed
	Col      int          `json:"col"`      // 1-indexed
	Snippet  string       `json:"snippet,omitempty"`
}

// Lint 静态扫描脚本源码。返 issues 列表（可能为空）。
//
// **重要**：源码未通过 Engine.Compile 时也能 Lint（best-effort）— 但语法错
// 会让 syntax.LegacyFile 直接 fail，这种情况返 nil + error。caller 应该把
// compile error 和 lint issues 合并展示。
func Lint(scriptID, code string) ([]LintIssue, error) {
	opts := &syntax.FileOptions{}
	f, err := opts.Parse(scriptID+".star", []byte(code), 0)
	if err != nil {
		return nil, fmt.Errorf("parse for lint: %w", err)
	}
	l := &linter{lines: strings.Split(code, "\n")}
	l.checkFile(f)
	return l.issues, nil
}

type linter struct {
	issues   []LintIssue
	lines    []string
	loopDepth int
	checkDef *syntax.DefStmt // 找到的 check 函数
}

func (l *linter) emit(code string, sev LintSeverity, msg string, pos syntax.Position) {
	snippet := ""
	idx := int(pos.Line) - 1
	if idx >= 0 && idx < len(l.lines) {
		snippet = strings.TrimSpace(l.lines[idx])
	}
	l.issues = append(l.issues, LintIssue{
		Code: code, Severity: sev, Message: msg,
		Line: int(pos.Line), Col: int(pos.Col), Snippet: snippet,
	})
}

// checkFile 顶层入口。
func (l *linter) checkFile(f *syntax.File) {
	// 找 def check
	for _, stmt := range f.Stmts {
		if def, ok := stmt.(*syntax.DefStmt); ok && def.Name.Name == "check" {
			l.checkDef = def
		}
	}
	if l.checkDef != nil {
		l.checkCheckSignature(l.checkDef)
		l.walkBody(l.checkDef.Body)
	}
	if len(l.lines) > 200 {
		l.emit("I001", SeverityInfo,
			fmt.Sprintf("script length %d lines > 200, consider splitting into helper modules via load()", len(l.lines)),
			syntax.Position{Line: 1, Col: 1})
	}
}

// checkCheckSignature E001 + E002 + W004
func (l *linter) checkCheckSignature(def *syntax.DefStmt) {
	if len(def.Params) != 1 {
		l.emit("E001", SeverityError,
			fmt.Sprintf("check() must take exactly 1 parameter (ctx), got %d", len(def.Params)),
			def.Def)
		return
	}
	param, ok := def.Params[0].(*syntax.Ident)
	if !ok {
		// 默认参数 / *args / **kwargs 都不允许
		l.emit("E001", SeverityError,
			"check() parameter must be a simple identifier 'ctx', not a default/varargs",
			def.Def)
		return
	}
	if param.Name != "ctx" {
		l.emit("E001", SeverityError,
			fmt.Sprintf("check() parameter must be named 'ctx' (engine injects ctx by position but convention is enforced), got %q", param.Name),
			param.NamePos)
	}

	// W004: ctx 没在 body 里被引用
	if !nameUsedInBody(def.Body, "ctx") {
		l.emit("W004", SeverityWarn,
			"check() defines 'ctx' parameter but never uses it; if intentional rename to '_'",
			param.NamePos)
	}

	// E002: return 检查 — 看到 return 必须是 list 或 list comprehension
	checkReturnIsList(def.Body, l)
}

// walkBody 递归遍历语句块，跟踪 loopDepth 检测嵌套循环 + 内部 expensive op。
func (l *linter) walkBody(stmts []syntax.Stmt) {
	for _, s := range stmts {
		l.walkStmt(s)
	}
}

func (l *linter) walkStmt(s syntax.Stmt) {
	switch n := s.(type) {
	case *syntax.ForStmt:
		l.loopDepth++
		if l.loopDepth >= 2 {
			l.emit("W001", SeverityWarn,
				fmt.Sprintf("nested for loop (depth=%d) suggests O(N^k) complexity; consider grouping with dict or 2-pass approach", l.loopDepth),
				n.For)
		}
		l.walkExpr(n.X)
		l.walkBody(n.Body)
		l.loopDepth--
	case *syntax.WhileStmt:
		l.loopDepth++
		l.walkExpr(n.Cond)
		l.walkBody(n.Body)
		l.loopDepth--
	case *syntax.IfStmt:
		l.walkExpr(n.Cond)
		l.walkBody(n.True)
		l.walkBody(n.False)
	case *syntax.ExprStmt:
		l.walkExpr(n.X)
	case *syntax.AssignStmt:
		l.walkExpr(n.LHS)
		l.walkExpr(n.RHS)
	case *syntax.ReturnStmt:
		if n.Result != nil {
			l.walkExpr(n.Result)
		}
	case *syntax.DefStmt:
		// nested def — 走 body
		l.walkBody(n.Body)
	}
}

// walkExpr 检测 loop 里调 expensive op (W002)。
func (l *linter) walkExpr(e syntax.Expr) {
	switch n := e.(type) {
	case *syntax.CallExpr:
		if l.loopDepth > 0 && isExpensiveCall(n) {
			l.emit("W002", SeverityWarn,
				fmt.Sprintf("calling %s inside a loop is expensive (each call is a Redis/HTTP roundtrip); batch outside the loop or limit iterations", callDisplay(n)),
				n.Lparen)
		}
		l.walkExpr(n.Fn)
		for _, a := range n.Args {
			l.walkExpr(a)
		}
	case *syntax.BinaryExpr:
		l.walkExpr(n.X)
		l.walkExpr(n.Y)
	case *syntax.UnaryExpr:
		l.walkExpr(n.X)
	case *syntax.ListExpr:
		for _, elem := range n.List {
			l.walkExpr(elem)
		}
	case *syntax.DictExpr:
		for _, ent := range n.List {
			if kv, ok := ent.(*syntax.DictEntry); ok {
				l.walkExpr(kv.Key)
				l.walkExpr(kv.Value)
			}
		}
	case *syntax.IndexExpr:
		l.walkExpr(n.X)
		l.walkExpr(n.Y)
	case *syntax.DotExpr:
		l.walkExpr(n.X)
	case *syntax.ParenExpr:
		l.walkExpr(n.X)
	case *syntax.Comprehension:
		l.walkExpr(n.Body)
		for _, c := range n.Clauses {
			switch cl := c.(type) {
			case *syntax.ForClause:
				l.walkExpr(cl.X)
			case *syntax.IfClause:
				l.walkExpr(cl.Cond)
			}
		}
	}
}

// isExpensiveCall 检测 ctx.scan_index / ctx.get_by_index / ctx.http_get / http_get(...) 等。
func isExpensiveCall(n *syntax.CallExpr) bool {
	switch fn := n.Fn.(type) {
	case *syntax.DotExpr:
		// ctx.scan_index / ctx.http_get
		switch fn.Name.Name {
		case "scan_index", "get_by_index", "http_get", "get":
			return true
		}
	case *syntax.Ident:
		// http_get / scan_index 直接调（少见，应通过 ctx）
		switch fn.Name {
		case "http_get", "scan_index":
			return true
		}
	}
	return false
}

func callDisplay(n *syntax.CallExpr) string {
	switch fn := n.Fn.(type) {
	case *syntax.DotExpr:
		if id, ok := fn.X.(*syntax.Ident); ok {
			return id.Name + "." + fn.Name.Name + "()"
		}
		return "." + fn.Name.Name + "()"
	case *syntax.Ident:
		return fn.Name + "()"
	}
	return "<call>"
}

// nameUsedInBody 简单 grep：判断 body 里是否引用了 name 标识符。
func nameUsedInBody(stmts []syntax.Stmt, name string) bool {
	used := false
	syntax.Walk(blockNode(stmts), func(n syntax.Node) bool {
		if id, ok := n.(*syntax.Ident); ok && id.Name == name {
			used = true
			return false
		}
		return true
	})
	return used
}

// blockNode 把 []Stmt 包装成 syntax.Node 让 Walk 能进入。
type blockNode []syntax.Stmt

func (b blockNode) Span() (start, end syntax.Position) {
	if len(b) == 0 {
		return
	}
	s, _ := b[0].Span()
	_, e := b[len(b)-1].Span()
	return s, e
}

// checkReturnIsList E002：找所有 return 语句，必须 return list/comprehension。
//
// 不报错的情况：
//   - return [...]                  (list 字面量)
//   - return [x for x in y if ...]  (list comprehension)
//   - return diffs                  (var；运行时再检查)
//   - return func_call()            (var；运行时再检查)
//   - 没有 return 语句               (Starlark 隐式返 None — 需要 explicit return [])
//
// 报错的情况：
//   - return {...}     (dict — 99% 是写错了)
//   - return None
//   - return 0 / "abc"
//   - return x, y      (tuple — 99% 是写错了)
func checkReturnIsList(stmts []syntax.Stmt, l *linter) {
	hasReturn := false
	syntax.Walk(blockNode(stmts), func(n syntax.Node) bool {
		ret, ok := n.(*syntax.ReturnStmt)
		if !ok {
			return true
		}
		hasReturn = true
		if ret.Result == nil {
			l.emit("E002", SeverityError,
				"check() must return a list of diffs; bare 'return' returns None",
				ret.Return)
			return true
		}
		switch r := ret.Result.(type) {
		case *syntax.ListExpr, *syntax.Comprehension:
			// OK
		case *syntax.Ident, *syntax.CallExpr, *syntax.IndexExpr:
			// 运行期再检查（var 可能是 list）
		case *syntax.DictExpr:
			l.emit("E002", SeverityError,
				"check() must return a list of diffs (a dict is treated as 1 diff and likely a typo — wrap in [...] if intentional)",
				r.Lbrace)
		case *syntax.TupleExpr:
			l.emit("E002", SeverityError,
				"check() must return a list of diffs, not a tuple — change parentheses to brackets",
				ret.Return)
		case *syntax.Literal:
			l.emit("E002", SeverityError,
				"check() must return a list of diffs, got literal value",
				ret.Return)
		}
		return true
	})
	if !hasReturn {
		// 没 return 等于隐式 return None
		if l.checkDef != nil {
			l.emit("E002", SeverityError,
				"check() has no return statement; add 'return diffs' or 'return []'",
				l.checkDef.Def)
		}
	}
}
