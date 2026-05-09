// symbols.go — 给 admin web autocomplete provider 用的脚本符号自省。
//
// 端点 GET /api/v1/script/symbols 返这个结构的 JSON，前端 Monaco editor 的
// completionItemProvider 消费它，提供：
//
//   - load("@<module>", ...) 第一个参数的 module 名补齐
//   - load("@json", "...") 第二个参数的成员名补齐
//   - ctx.<member> 补齐
//   - event.<member> / events.<member> 补齐

package script

// Symbols 脚本环境的全部可用符号清单。
type Symbols struct {
	// Modules 已注册的所有 builtin / host module（脚本通过 load("@<name>") 引入）。
	Modules []ModuleSymbols `json:"modules"`
	// Ctx ctx 对象（def check(ctx)）的属性 / 方法清单。
	Ctx []SymbolInfo `json:"ctx"`
	// Event 单个 event 对象的属性 / 方法清单。
	Event []SymbolInfo `json:"event"`
	// EventList event 列表对象的属性 / 方法清单。
	EventList []SymbolInfo `json:"event_list"`
}

// ModuleSymbols 单个 module 暴露的所有顶层符号。
type ModuleSymbols struct {
	Name    string       `json:"name"`
	Members []SymbolInfo `json:"members"`
}

// CollectSymbols 给 engine 拉一份完整 symbols 快照。
//
// 调用频率：admin web 编辑器加载时调一次，后续 RegisterModule 后通过
// SSE / websocket 推送变更（暂未实现，每次保存编辑器 reload）。
func CollectSymbols(e *Engine) Symbols {
	out := Symbols{
		Ctx:       infosFromAttrNames(CtxAttrNames(), ctxAttrTypes()),
		Event:     infosFromAttrNames(EventAttrNames(), eventAttrTypes()),
		EventList: infosFromAttrNames(EventListAttrNames(), eventListAttrTypes()),
	}
	if e == nil {
		return out
	}
	for _, name := range e.ModuleNames() {
		out.Modules = append(out.Modules, ModuleSymbols{
			Name:    name,
			Members: e.ModuleSymbols(name),
		})
	}
	return out
}

func infosFromAttrNames(names []string, types map[string]string) []SymbolInfo {
	out := make([]SymbolInfo, 0, len(names))
	for _, n := range names {
		t := types[n]
		if t == "" {
			t = "method"
		}
		out = append(out, SymbolInfo{Name: n, Type: t})
	}
	return out
}

// ctxAttrTypes 为 starlarkContext.AttrNames 提供类型注释（给前端展示）。
func ctxAttrTypes() map[string]string {
	return map[string]string{
		"now":         "string (RFC3339)",
		"params":      "dict[str,str]",
		"scan_index":  "method (idx_name, prefix='', limit=1000) → list[str]",
		"get_by_index": "method (idx_name, value) → EventList",
		"get":         "method (service, table, pk) → Event | None",
		"http_get":    "method (url, timeout_sec=5) → dict {status, body}",
		"log_info":    "method (msg, **kv)",
		"log_warn":    "method (msg, **kv)",
		"log_error":   "method (msg, **kv)",
	}
}

func eventAttrTypes() map[string]string {
	return map[string]string{
		"service":   "string",
		"table":     "string",
		"pk":        "string",
		"timestamp": "string (RFC3339)",
		"int":       "method (col) → int",
		"str":       "method (col) → string",
		"get":       "method (col, default='') → string",
		"data":      "dict[str,str]",
	}
}

func eventListAttrTypes() map[string]string {
	return map[string]string{
		"find":     "method (service, table) → Event | None",
		"find_all": "method (service, table) → EventList",
		"len":      "int",
	}
}
