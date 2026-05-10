// Package dsl — 高阶对账模板引擎。
//
// **运营痛点**：写 Starlark 还是有门槛（变量、循环、dict 操作）。80% 对账场景
// 是 4 类标准模式：
//
//   1. amount_match     A 表 amount == B 表 amount？
//   2. missing_leg      A 表存在但 B 表缺？
//   3. state_consistency A 表 status=succeeded 但 B 表 status≠succeeded？
//   4. count_match      A 表行数 / 总额 vs B 表（按时间窗）
//
// 模板引擎让运营在 admin web 表单里选模板 + 填字段，后端 Render 出 Starlark
// 代码，走标准 save / dry-run / publish 流程。
//
// 输出代码可读，运营点 "View Code" 看到完整 Starlark 后随时 Edit 直接转
// "高级模式" 自由改。
package dsl

import (
	"bytes"
	"fmt"
	"text/template"
)

// Template 模板元信息（admin web 列模板时返）。
type Template struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Fields      []FieldDef         `json:"fields"`
}

// FieldDef 模板的一个参数。
type FieldDef struct {
	Name     string `json:"name"`     // 表单 input id
	Label    string `json:"label"`    // 表单展示标签
	Type     string `json:"type"`     // string / number / select / event_ref
	Required bool   `json:"required"`
	Options  []string `json:"options,omitempty"`  // type=select 时
	Default  string   `json:"default,omitempty"`
	Hint     string   `json:"hint,omitempty"`     // placeholder / 输入提示
}

// All 返回内置模板清单（GET /api/v1/dsl/templates 用）。
func All() []Template {
	return []Template{templateAmountMatch(), templateMissingLeg(), templateStateConsistency(), templateCountMatch()}
}

// Render 模板 + params → Starlark 代码字符串。
//
// caller (admin handler) 已经 validate 过 params；这里假设 fields 都齐。
func Render(templateID string, params map[string]string) (string, error) {
	t := findTemplate(templateID)
	if t == nil {
		return "", fmt.Errorf("unknown template %q", templateID)
	}
	// 校验 required 字段
	for _, f := range t.Fields {
		if f.Required && params[f.Name] == "" {
			return "", fmt.Errorf("required field %q missing", f.Name)
		}
		if params[f.Name] == "" && f.Default != "" {
			params[f.Name] = f.Default
		}
	}
	tpl, err := template.New(templateID).Parse(starlarkTmpl(templateID))
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, params); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

func findTemplate(id string) *Template {
	for _, t := range All() {
		if t.ID == id {
			return &t
		}
	}
	return nil
}

// ─── 内置模板定义 ──────────────────────────────────────────────────────

func templateAmountMatch() Template {
	return Template{
		ID:          "amount_match",
		Name:        "金额一致性对账",
		Description: "比较两表/两服务同一逻辑实体的金额字段是否一致（差异容忍可配）",
		Fields: []FieldDef{
			{Name: "idx_name", Label: "关联索引列", Type: "select", Required: true,
				Options: []string{"pi_id", "order_id", "merchant_id", "trace_id"},
				Hint: "用哪个 index 把两边的事件关联起来"},
			{Name: "src_svc", Label: "源服务", Type: "string", Required: true,
				Hint: "如 order-core"},
			{Name: "src_table", Label: "源表", Type: "string", Required: true,
				Hint: "如 payment_intents"},
			{Name: "dst_svc", Label: "目标服务", Type: "string", Required: true,
				Hint: "如 payment-channel"},
			{Name: "dst_table", Label: "目标表", Type: "string", Required: true,
				Hint: "如 acquirer_tx"},
			{Name: "amount_col", Label: "金额列名", Type: "string", Required: true, Default: "amount"},
			{Name: "tolerance", Label: "容忍差异（minor units）", Type: "number", Default: "0",
				Hint: "0=严格相等；100=允许差 1 元（PHP cent）"},
		},
	}
}

func templateMissingLeg() Template {
	return Template{
		ID:          "missing_leg",
		Name:        "缺腿检查",
		Description: "A 表有但 B 表缺事件（或反之）；给定时间窗内扫描",
		Fields: []FieldDef{
			{Name: "idx_name", Label: "关联索引列", Type: "select", Required: true,
				Options: []string{"pi_id", "order_id", "merchant_id"}},
			{Name: "src_svc", Label: "源服务", Type: "string", Required: true},
			{Name: "src_table", Label: "源表", Type: "string", Required: true},
			{Name: "dst_svc", Label: "目标服务", Type: "string", Required: true},
			{Name: "dst_table", Label: "目标表", Type: "string", Required: true},
			{Name: "scan_limit", Label: "扫描上限", Type: "number", Default: "5000"},
		},
	}
}

func templateStateConsistency() Template {
	return Template{
		ID:          "state_consistency",
		Name:        "状态一致性对账",
		Description: "A.status==X 时 B.status 必须==Y（如 PI=succeeded 时 charge 也必须 succeeded）",
		Fields: []FieldDef{
			{Name: "idx_name", Label: "关联索引列", Type: "select", Required: true,
				Options: []string{"pi_id", "order_id"}},
			{Name: "src_svc", Label: "源服务", Type: "string", Required: true},
			{Name: "src_table", Label: "源表", Type: "string", Required: true},
			{Name: "src_state_col", Label: "源 status 列", Type: "string", Default: "status"},
			{Name: "src_state_val", Label: "源 status 触发值", Type: "string", Required: true,
				Hint: "如 succeeded"},
			{Name: "dst_svc", Label: "目标服务", Type: "string", Required: true},
			{Name: "dst_table", Label: "目标表", Type: "string", Required: true},
			{Name: "dst_state_col", Label: "目标 status 列", Type: "string", Default: "status"},
			{Name: "dst_state_val", Label: "目标 status 期望值", Type: "string", Required: true},
		},
	}
}

func templateCountMatch() Template {
	return Template{
		ID:          "count_match",
		Name:        "笔数 / 总额对账",
		Description: "时间窗内 A 表行数 / 总额 vs B 表",
		Fields: []FieldDef{
			{Name: "idx_name", Label: "扫描索引", Type: "select", Required: true,
				Options: []string{"pi_id", "order_id"}},
			{Name: "src_svc", Label: "源服务", Type: "string", Required: true},
			{Name: "src_table", Label: "源表", Type: "string", Required: true},
			{Name: "dst_svc", Label: "目标服务", Type: "string", Required: true},
			{Name: "dst_table", Label: "目标表", Type: "string", Required: true},
			{Name: "amount_col", Label: "金额列（可选，留空仅核行数）", Type: "string", Default: ""},
		},
	}
}

// ─── Starlark 代码模板 ─────────────────────────────────────────────────

func starlarkTmpl(id string) string {
	switch id {
	case "amount_match":
		return amountMatchTpl
	case "missing_leg":
		return missingLegTpl
	case "state_consistency":
		return stateConsistencyTpl
	case "count_match":
		return countMatchTpl
	}
	return ""
}

const amountMatchTpl = `# Auto-generated by DSL template "amount_match".
# Edit freely — saving here switches script to "advanced" mode (no longer
# round-trippable to template form).

def check(ctx):
    diffs = []
    keys = ctx.scan_index("{{.idx_name}}", "", 5000)
    tolerance = {{.tolerance}}
    for k in keys:
        events = ctx.get_by_index("{{.idx_name}}", k)
        src = events.find("{{.src_svc}}", "{{.src_table}}")
        dst = events.find("{{.dst_svc}}", "{{.dst_table}}")
        if not src or not dst:
            continue
        a = src.int("{{.amount_col}}")
        b = dst.int("{{.amount_col}}")
        delta = a - b
        if delta < 0:
            delta = -delta
        if delta > tolerance:
            diffs.append({
                "type": "amount_mismatch",
                "key": k,
                "want": a,
                "got": b,
                "detail": {
                    "src": "{{.src_svc}}/{{.src_table}}",
                    "dst": "{{.dst_svc}}/{{.dst_table}}",
                    "delta": delta,
                    "tolerance": tolerance,
                },
            })
    ctx.log_info("amount_match done", "scanned", len(keys), "diffs", len(diffs))
    return diffs
`

const missingLegTpl = `# Auto-generated by DSL template "missing_leg".

def check(ctx):
    diffs = []
    keys = ctx.scan_index("{{.idx_name}}", "", {{.scan_limit}})
    for k in keys:
        events = ctx.get_by_index("{{.idx_name}}", k)
        src = events.find("{{.src_svc}}", "{{.src_table}}")
        dst = events.find("{{.dst_svc}}", "{{.dst_table}}")
        if src and not dst:
            diffs.append({
                "type": "missing_leg",
                "key": k,
                "detail": {
                    "have": "{{.src_svc}}/{{.src_table}}",
                    "missing": "{{.dst_svc}}/{{.dst_table}}",
                },
            })
        elif dst and not src:
            diffs.append({
                "type": "missing_leg",
                "key": k,
                "detail": {
                    "have": "{{.dst_svc}}/{{.dst_table}}",
                    "missing": "{{.src_svc}}/{{.src_table}}",
                },
            })
    ctx.log_info("missing_leg done", "scanned", len(keys), "diffs", len(diffs))
    return diffs
`

const stateConsistencyTpl = `# Auto-generated by DSL template "state_consistency".

def check(ctx):
    diffs = []
    keys = ctx.scan_index("{{.idx_name}}", "", 5000)
    for k in keys:
        events = ctx.get_by_index("{{.idx_name}}", k)
        src = events.find("{{.src_svc}}", "{{.src_table}}")
        dst = events.find("{{.dst_svc}}", "{{.dst_table}}")
        if not src or not dst:
            continue
        if src.str("{{.src_state_col}}") != "{{.src_state_val}}":
            continue  # 触发条件不满足；跳过
        if dst.str("{{.dst_state_col}}") != "{{.dst_state_val}}":
            diffs.append({
                "type": "state_inconsistent",
                "key": k,
                "want": "{{.dst_state_val}}",
                "got": dst.str("{{.dst_state_col}}"),
                "detail": {
                    "src": "{{.src_svc}}/{{.src_table}}: {{.src_state_col}}={{.src_state_val}}",
                    "dst": "{{.dst_svc}}/{{.dst_table}}: " + "{{.dst_state_col}}=" + dst.str("{{.dst_state_col}}"),
                },
            })
    ctx.log_info("state_consistency done", "scanned", len(keys), "diffs", len(diffs))
    return diffs
`

const countMatchTpl = `# Auto-generated by DSL template "count_match".

def check(ctx):
    keys = ctx.scan_index("{{.idx_name}}", "", 10000)
    src_count = 0
    dst_count = 0
    src_amount = 0
    dst_amount = 0
    amount_col = "{{.amount_col}}"
    for k in keys:
        events = ctx.get_by_index("{{.idx_name}}", k)
        src = events.find("{{.src_svc}}", "{{.src_table}}")
        dst = events.find("{{.dst_svc}}", "{{.dst_table}}")
        if src:
            src_count += 1
            if amount_col:
                src_amount += src.int(amount_col)
        if dst:
            dst_count += 1
            if amount_col:
                dst_amount += dst.int(amount_col)
    diffs = []
    if src_count != dst_count:
        diffs.append({
            "type": "count_mismatch",
            "key": "{{.src_svc}}/{{.src_table}}_vs_{{.dst_svc}}/{{.dst_table}}",
            "want": src_count,
            "got": dst_count,
        })
    if amount_col and src_amount != dst_amount:
        diffs.append({
            "type": "amount_total_mismatch",
            "key": "{{.src_svc}}/{{.src_table}}_vs_{{.dst_svc}}/{{.dst_table}}",
            "want": src_amount,
            "got": dst_amount,
        })
    ctx.log_info("count_match done", "scanned", len(keys),
        "src_count", src_count, "dst_count", dst_count)
    return diffs
`
