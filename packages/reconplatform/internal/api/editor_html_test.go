package api

import (
	"strings"
	"testing"
)

// editorHTML 一个 SPA。直接跑浏览器测试需要 webdriver；这里只做 string-level
// 兜底，确保关键集成点没在重构里悄悄丢掉：
//   - Monaco AMD loader URL 正确
//   - 拉 /api/v1/script/symbols 做 autocomplete
//   - 用 Starlark 风格的 starter 模板（不是老的 Go yaegi 模板）
//   - 4 个 completion 触发场景都注册了
func TestEditorHTML_HasMonacoIntegration(t *testing.T) {
	cases := []struct {
		name     string
		contains string
	}{
		{"monaco loader URL", "monaco-editor@0.46.0/min/vs/loader.js"},
		{"monaco editor.main.css", "monaco-editor@0.46.0/min/vs/editor/editor.main.css"},
		{"symbols endpoint", "/api/v1/script/symbols"},
		{"completion provider registered", "registerCompletionItemProvider"},
		// 4 个 completion 触发分支
		{"trigger: load module", `load\s*\(\s*"@`},
		{"trigger: load member", `load\s*\(\s*"@([a-zA-Z0-9_]+)"`},
		{"trigger: ctx.attr", `\\bctx\.\\w*$`},
		{"trigger: var.attr (events)", "event_list"},
		// 编辑器实例化
		{"editor.create", "monaco.editor.create"},
		// 快捷键 hook
		{"Cmd+S binding", "KeyCode.KeyS"},
		// Starlark starter（不要还残留 yaegi 风格的 import "recon"）
		{"starlark starter has def check(ctx)", "def check(ctx)"},
		{"starlark starter has return diffs", "return diffs"},
	}
	for _, c := range cases {
		if !strings.Contains(editorHTMLContent, c.contains) {
			t.Errorf("[%s] editorHTMLContent missing %q", c.name, c.contains)
		}
	}
}

// 老 yaegi 风格的关键字应已全部清掉（避免运营 copy-paste 老示例）。
func TestEditorHTML_NoYaegiResidue(t *testing.T) {
	bad := []string{
		`package main`,
		`func Check(ctx *recon.Context)`,
		`*recon.Result`,
		`import "recon"`,
		`ctx.AddCompare(`, // yaegi-only API
	}
	for _, s := range bad {
		if strings.Contains(editorHTMLContent, s) {
			t.Errorf("editorHTMLContent still references yaegi-era token %q", s)
		}
	}
}
