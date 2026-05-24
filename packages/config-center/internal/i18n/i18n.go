// Package i18n — config-center admin 后台的服务端 i18n 引擎。
//
// 设计要点：
//   1. 双语 message 表用 //go:embed 嵌进二进制，进程启动一次性加载到内存。
//   2. T(lang, key) 返回原文（无插值，最常用）。
//   3. Tf(lang, key, args ...any) 走 text/template 单 key 渲染，支持 {{.Field}}
//      或 {{.K}}{{.V}} 等模板插值。args 是 key/value 对（"NS","order-core","Key","x"）。
//   4. 缺 key fallback：currentLang → en-US → 字面 key（便于联调发现漏 key）。
//   5. 不依赖任何第三方 i18n 库（avoid go-i18n 的 toml 复杂度）。
//
// 用法（template 里）：
//   {{T .Lang "page.index.title"}}
//   {{Tf .Lang "page.listKeys.title" "NS" .Namespace}}
//
// 用法（Go 代码里）：
//   title := i18n.T("zh-CN", "page.edit.cardHead")
package i18n

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"sync"
	"text/template"
)

//go:embed messages_zh.json
var rawZh []byte

//go:embed messages_en.json
var rawEn []byte

// Lang 常量 — 跟前端 SUPPORTED_LANGUAGES 对齐（zh-CN / en-US）。
const (
	LangZH = "zh-CN"
	LangEN = "en-US"
)

// DefaultLang 缺省语言（既是 fallback，也是首次访问默认）。
const DefaultLang = LangZH

var (
	mu       sync.RWMutex
	messages = map[string]map[string]string{}

	// 单 key 模板的解析缓存：避免每次 Tf 都重新 Parse。
	tplCache sync.Map // key: "lang:key" → *template.Template
)

func init() {
	loadOne(LangZH, rawZh)
	loadOne(LangEN, rawEn)
}

func loadOne(lang string, raw []byte) {
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		// embed 出来的 JSON 编译期就应被 fmt 工具校验；运行期 panic 是 fail-fast。
		panic("i18n: load " + lang + " failed: " + err.Error())
	}
	mu.Lock()
	messages[lang] = m
	mu.Unlock()
}

// NormalizeLang 把任意输入（cookie / query / Accept-Language）规整成支持的语言码。
// 只看前缀：以 "en" 开头视为 en-US，其它一律 zh-CN。
func NormalizeLang(s string) string {
	if len(s) >= 2 && (s[0] == 'e' || s[0] == 'E') && (s[1] == 'n' || s[1] == 'N') {
		return LangEN
	}
	return LangZH
}

// Supported 当前支持的语言列表（顺序与前端一致）。
func Supported() []struct{ Code, Label string } {
	return []struct{ Code, Label string }{
		{LangZH, "中文"},
		{LangEN, "English"},
	}
}

// lookup 按 lang → en-US → 字面 key 顺序找消息。
func lookup(lang, key string) string {
	mu.RLock()
	defer mu.RUnlock()
	if m, ok := messages[lang]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	if lang != LangEN {
		if m, ok := messages[LangEN]; ok {
			if v, ok := m[key]; ok {
				return v
			}
		}
	}
	return key
}

// T 取 message 原文。无插值。
//
// 模板里调用：{{T .Lang "page.index.title"}}
func T(lang, key string) string {
	return lookup(lang, key)
}

// Tf 取 message 并按 args（key, value 对）做 text/template 插值。
//
// 模板里调用：{{Tf .Lang "page.listKeys.title" "NS" .Namespace}}
// Go 里调用：i18n.Tf("zh-CN", "page.keyDetail.title", "NS", ns, "Key", key)
//
// args 必须成对；奇数被忽略。key 必须是合法 Go 标识符（因为 text/template 用作字段名）。
func Tf(lang, key string, args ...any) string {
	raw := lookup(lang, key)
	if len(args) < 2 {
		return raw
	}
	cacheKey := lang + ":" + key
	var tpl *template.Template
	if cached, ok := tplCache.Load(cacheKey); ok {
		tpl = cached.(*template.Template)
	} else {
		parsed, err := template.New(key).Parse(raw)
		if err != nil {
			// 模板里有语法错（很罕见，缺漏 `}}`）→ 直接返回原文，不挂掉页面。
			return raw
		}
		tplCache.Store(cacheKey, parsed)
		tpl = parsed
	}
	data := map[string]any{}
	for i := 0; i+1 < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok {
			continue
		}
		data[k] = args[i+1]
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return raw
	}
	return buf.String()
}
