// yaml.go — 极简 YAML loader.
//
// 不引 yaml.v3 (避免 dep); 只支持基本 key:value 和嵌套, 给 scaffold config 用足够.
// 真服务想要更强 yaml 自己装 yaml.v3.

package scaffold

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// loadYAMLFile 极简 yaml 解析 — 只支持: key: value, 嵌套 (2 空格缩进), time.Duration string.
//
// 复杂 yaml (anchors / multi-doc / arrays) 用 yaml.v3 替换.
func loadYAMLFile(path string, out interface{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	return parseYAML(string(data), out)
}

func parseYAML(src string, out interface{}) error {
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return errors.New("scaffold/yaml: out must be non-nil pointer")
	}
	target := rv.Elem()

	type frame struct {
		v      reflect.Value
		indent int
	}
	stack := []frame{{v: target, indent: -1}}

	lines := strings.Split(src, "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		content := strings.TrimSpace(line)

		// pop stack 到对应 indent
		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].v

		colon := strings.Index(content, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(content[:colon])
		val := strings.TrimSpace(content[colon+1:])

		field := findFieldByYAMLTag(parent, key)
		if !field.IsValid() {
			continue
		}

		if val == "" {
			// 嵌套, 入栈
			if field.Kind() == reflect.Struct {
				stack = append(stack, frame{v: field, indent: indent})
			}
			continue
		}

		setFieldFromString(field, val)
	}
	return nil
}

// findFieldByYAMLTag 按 yaml tag 找字段
func findFieldByYAMLTag(v reflect.Value, key string) reflect.Value {
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" {
			tag = strings.ToLower(f.Name)
		}
		// strip 选项 (",omitempty" 等)
		if comma := strings.Index(tag, ","); comma >= 0 {
			tag = tag[:comma]
		}
		if tag == key {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

// setFieldFromString 把 yaml 值 string 转字段类型.
func setFieldFromString(f reflect.Value, s string) {
	// 去引号
	s = strings.Trim(s, `"'`)
	switch f.Kind() {
	case reflect.String:
		f.SetString(s)
	case reflect.Bool:
		b := s == "true" || s == "yes" || s == "1"
		f.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// time.Duration 特殊处理
		if f.Type() == reflect.TypeOf(time.Duration(0)) {
			if d, err := time.ParseDuration(s); err == nil {
				f.SetInt(int64(d))
				return
			}
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			f.SetInt(n)
		}
	case reflect.Float32, reflect.Float64:
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			f.SetFloat(n)
		}
	}
}
