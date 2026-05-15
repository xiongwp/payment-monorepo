// filter_expr.go — MF-3: 真正的 trigger filter 表达式求值.
//
// 支持的语法:
//   字面量:     数字 (int/float) / 单/双引号字符串 / true / false
//   字段访问:   amount_minor / currency / charge_id / merchant_id / event /
//               attr.<key>  (从 ev.Attributes 取, 兼容 "merchant.tier" 这种点路径,
//               按整个 key "merchant.tier" 在 Attributes 中找)
//   比较:       ==  !=  <  <=  >  >=
//   逻辑:       and  or  not   (不区分大小写)
//   成员:       <expr> in (<list>)    e.g.  currency in ('USD', 'EUR')
//   分组:       ( ... )
//
// 例子 (都合法):
//   merchant.tier == 'marketplace'
//   amount_minor > 10000 and currency in ('USD', 'EUR')
//   not (event == 'refund.completed') and attr.country == 'US'
//
// 安全:
//   - 单次表达式 token 数上限 256, 防 DoS;
//   - 求值无副作用, 不调用任何 IO;
//   - 不支持赋值 / 函数调用 / 字符串拼接.
//
// 旧 simpleFilter (= / !=) 仍可识别, 走兼容路径.
package workflow

import (
	"fmt"
	"strconv"
	"strings"
)

// EvalFilter 求值 expr 在 ev 上下文里是否成立.
//
// expr 空 → true (调用方一般不会传空, 但稳健起见).
// 解析 / 求值出错 → false + caller 应在 log 里看到 error (本函数自身只返 bool 简化).
// 想看错误细节用 EvalFilterErr.
func EvalFilter(expr string, ev BusinessEvent) bool {
	ok, _ := EvalFilterErr(expr, ev)
	return ok
}

// EvalFilterErr 表达式求值 + 返错误细节 (admin /api/moneyflow/filter/test 用).
func EvalFilterErr(expr string, ev BusinessEvent) (bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true, nil
	}
	// 兼容老 simpleFilter "k=v" / "k!=v" — 把单 "=" 转 "==" 以走新解析.
	// 检查不是 != / == / >= / <= 的孤立 "=".
	if hasBareEq(expr) {
		expr = upgradeBareEq(expr)
	}

	tokens, err := tokenize(expr)
	if err != nil {
		return false, err
	}
	if len(tokens) == 0 {
		return true, nil
	}
	p := &parser{toks: tokens, ev: ev}
	v, err := p.parseOr()
	if err != nil {
		return false, err
	}
	if p.pos != len(tokens) {
		return false, fmt.Errorf("trailing tokens at %d: %v", p.pos, tokens[p.pos:])
	}
	return truthy(v), nil
}

// ─── tokenizer ─────────────────────────────────────────────────────────

type tokKind int

const (
	tkIdent tokKind = iota
	tkNum
	tkStr
	tkLP
	tkRP
	tkComma
	tkEQ
	tkNE
	tkLT
	tkLE
	tkGT
	tkGE
	tkAND
	tkOR
	tkNOT
	tkIN
	tkBool
)

type token struct {
	kind tokKind
	val  string  // ident / 字符串内容 / 数字字面量
	num  float64 // tkNum 时
	b    bool    // tkBool 时
}

const maxTokens = 256

func tokenize(s string) ([]token, error) {
	out := make([]token, 0, 16)
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(':
			out = append(out, token{kind: tkLP})
			i++
		case c == ')':
			out = append(out, token{kind: tkRP})
			i++
		case c == ',':
			out = append(out, token{kind: tkComma})
			i++
		case c == '=' && i+1 < len(s) && s[i+1] == '=':
			out = append(out, token{kind: tkEQ})
			i += 2
		case c == '!' && i+1 < len(s) && s[i+1] == '=':
			out = append(out, token{kind: tkNE})
			i += 2
		case c == '<':
			if i+1 < len(s) && s[i+1] == '=' {
				out = append(out, token{kind: tkLE})
				i += 2
			} else {
				out = append(out, token{kind: tkLT})
				i++
			}
		case c == '>':
			if i+1 < len(s) && s[i+1] == '=' {
				out = append(out, token{kind: tkGE})
				i += 2
			} else {
				out = append(out, token{kind: tkGT})
				i++
			}
		case c == '\'' || c == '"':
			str, n, err := readString(s, i)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tkStr, val: str})
			i += n
		case c >= '0' && c <= '9' || (c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'):
			num, n, err := readNumber(s, i)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tkNum, num: num, val: s[i : i+n]})
			i += n
		case isIdentStart(c):
			id, n := readIdent(s, i)
			i += n
			low := strings.ToLower(id)
			switch low {
			case "and":
				out = append(out, token{kind: tkAND})
			case "or":
				out = append(out, token{kind: tkOR})
			case "not":
				out = append(out, token{kind: tkNOT})
			case "in":
				out = append(out, token{kind: tkIN})
			case "true":
				out = append(out, token{kind: tkBool, b: true})
			case "false":
				out = append(out, token{kind: tkBool, b: false})
			default:
				out = append(out, token{kind: tkIdent, val: id})
			}
		default:
			return nil, fmt.Errorf("unexpected char %q at %d", c, i)
		}
		if len(out) > maxTokens {
			return nil, fmt.Errorf("expression too long (>%d tokens)", maxTokens)
		}
	}
	return out, nil
}

func readString(s string, i int) (string, int, error) {
	quote := s[i]
	j := i + 1
	for j < len(s) {
		if s[j] == '\\' && j+1 < len(s) {
			j += 2
			continue
		}
		if s[j] == quote {
			// 处理转义 \' \" \\
			body := s[i+1 : j]
			body = strings.ReplaceAll(body, "\\\\", "\\")
			body = strings.ReplaceAll(body, "\\'", "'")
			body = strings.ReplaceAll(body, "\\\"", "\"")
			return body, (j - i + 1), nil
		}
		j++
	}
	return "", 0, fmt.Errorf("unterminated string at %d", i)
}

func readNumber(s string, i int) (float64, int, error) {
	j := i
	if s[j] == '-' {
		j++
	}
	for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
		j++
	}
	if j == i {
		return 0, 0, fmt.Errorf("bad number at %d", i)
	}
	v, err := strconv.ParseFloat(s[i:j], 64)
	if err != nil {
		return 0, 0, err
	}
	return v, j - i, nil
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}
func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '.'
}

func readIdent(s string, i int) (string, int) {
	j := i + 1
	for j < len(s) && isIdentCont(s[j]) {
		j++
	}
	return s[i:j], j - i
}

// ─── parser (recursive descent) ────────────────────────────────────────

type parser struct {
	toks []token
	pos  int
	ev   BusinessEvent
}

// parseOr := parseAnd ('or' parseAnd)*
func (p *parser) parseOr() (any, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek(tkOR) {
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = truthy(left) || truthy(right)
	}
	return left, nil
}

// parseAnd := parseNot ('and' parseNot)*
func (p *parser) parseAnd() (any, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.peek(tkAND) {
		p.pos++
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = truthy(left) && truthy(right)
	}
	return left, nil
}

// parseNot := 'not' parseNot | parseCmp
func (p *parser) parseNot() (any, error) {
	if p.peek(tkNOT) {
		p.pos++
		v, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return !truthy(v), nil
	}
	return p.parseCmp()
}

// parseCmp := parsePrimary (('==' | '!=' | '<' | '<=' | '>' | '>=' | 'in' '(' list ')') parsePrimary)?
func (p *parser) parseCmp() (any, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	if p.pos >= len(p.toks) {
		return left, nil
	}
	op := p.toks[p.pos].kind
	switch op {
	case tkEQ, tkNE, tkLT, tkLE, tkGT, tkGE:
		p.pos++
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		return cmp(left, right, op), nil
	case tkIN:
		p.pos++
		if !p.peek(tkLP) {
			return nil, fmt.Errorf("expected '(' after 'in'")
		}
		p.pos++
		items := []any{}
		for !p.peek(tkRP) {
			v, err := p.parsePrimary()
			if err != nil {
				return nil, err
			}
			items = append(items, v)
			if p.peek(tkComma) {
				p.pos++
			}
		}
		if !p.peek(tkRP) {
			return nil, fmt.Errorf("expected ')' in 'in' list")
		}
		p.pos++
		return containsAny(items, left), nil
	}
	return left, nil
}

// parsePrimary := number | string | bool | ident | '(' expr ')'
func (p *parser) parsePrimary() (any, error) {
	if p.pos >= len(p.toks) {
		return nil, fmt.Errorf("unexpected end of expr")
	}
	t := p.toks[p.pos]
	switch t.kind {
	case tkNum:
		p.pos++
		return t.num, nil
	case tkStr:
		p.pos++
		return t.val, nil
	case tkBool:
		p.pos++
		return t.b, nil
	case tkLP:
		p.pos++
		v, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.peek(tkRP) {
			return nil, fmt.Errorf("expected ')'")
		}
		p.pos++
		return v, nil
	case tkIdent:
		p.pos++
		return p.resolveIdent(t.val), nil
	}
	return nil, fmt.Errorf("unexpected token %v at %d", t, p.pos)
}

// resolveIdent 字段从 BusinessEvent 取.
//
// 内建字段:
//   event / charge_id / merchant_id / amount_minor / currency
// 其它: 按 ident (可含点) 整个去 ev.Attributes 中找.
//   特例: "attr.xxx" 拿 ev.Attributes["xxx"]
func (p *parser) resolveIdent(id string) any {
	switch id {
	case "event":
		return p.ev.Event
	case "charge_id":
		return p.ev.ChargeID
	case "merchant_id":
		return p.ev.MerchantID
	case "amount_minor":
		return float64(p.ev.AmountMinor)
	case "currency":
		return p.ev.Currency
	}
	// attr.xxx → ev.Attributes["xxx"]
	if strings.HasPrefix(id, "attr.") {
		return p.ev.Attributes[id[len("attr."):]]
	}
	// 整个 id (含点) 当 attribute key 查 (e.g. "merchant.tier")
	if v, ok := p.ev.Attributes[id]; ok {
		return v
	}
	return ""
}

func (p *parser) peek(k tokKind) bool {
	return p.pos < len(p.toks) && p.toks[p.pos].kind == k
}

// ─── 运行时辅助 ───────────────────────────────────────────────────────

// truthy 任何值转 bool: bool 直接; 字符串非空; 数字非 0; nil → false.
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case nil:
		return false
	}
	return true
}

// cmp 二元比较, 数字按浮点, 其它按字符串.
func cmp(a, b any, op tokKind) bool {
	// 都能转数字 → 数字比
	an, aok := asNum(a)
	bn, bok := asNum(b)
	if aok && bok {
		switch op {
		case tkEQ:
			return an == bn
		case tkNE:
			return an != bn
		case tkLT:
			return an < bn
		case tkLE:
			return an <= bn
		case tkGT:
			return an > bn
		case tkGE:
			return an >= bn
		}
	}
	// 字符串比较
	as, bs := fmt.Sprint(a), fmt.Sprint(b)
	switch op {
	case tkEQ:
		return as == bs
	case tkNE:
		return as != bs
	case tkLT:
		return as < bs
	case tkLE:
		return as <= bs
	case tkGT:
		return as > bs
	case tkGE:
		return as >= bs
	}
	return false
}

func asNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

func containsAny(items []any, v any) bool {
	for _, it := range items {
		if cmp(it, v, tkEQ) {
			return true
		}
	}
	return false
}

// ─── 兼容老 simpleFilter ("k=v") ────────────────────────────────────────

// hasBareEq 检测有没有孤立的 "=" (不是 == != >= <=).
func hasBareEq(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '=' {
			continue
		}
		// "==" / "!=" / ">=" / "<=" → 不算 bare
		if i > 0 && (s[i-1] == '=' || s[i-1] == '!' || s[i-1] == '>' || s[i-1] == '<') {
			continue
		}
		if i+1 < len(s) && s[i+1] == '=' {
			continue
		}
		return true
	}
	return false
}

// upgradeBareEq 把 bare "=" 替换成 "==".
func upgradeBareEq(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '=' &&
			!(i > 0 && (s[i-1] == '=' || s[i-1] == '!' || s[i-1] == '>' || s[i-1] == '<')) &&
			!(i+1 < len(s) && s[i+1] == '=') {
			b.WriteString("==")
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
