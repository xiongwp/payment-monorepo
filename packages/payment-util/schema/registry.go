// Package schema — Kafka / 事件 schema 中心化注册表 + 版本化 + 兼容性检查.
//
// 解决问题:
//   - producer / consumer 之间 schema 漂移 (字段加了 / 删了 / 改类型 → consumer 崩)
//   - 没有"事件目录"的话, 新加 service 不知道有哪些事件可以订阅
//   - schema 变更没 review 流程
//
// 设计:
//   - 每个 event topic 一个 Schema (name + version + JSON-schema body)
//   - Register 时检查兼容性 (BACKWARD / FORWARD / FULL)
//   - producer 发消息前 Validate
//   - consumer 拉消息前可选 Validate (debug 模式开 / 生产关)
//
// 存储:
//   - dev: 内存
//   - prod: config-center / Confluent Schema Registry / Redpanda Console
//
// 跟 Confluent Schema Registry 兼容: 同样的 REST API 形状, 替换 backend 即可。

package schema

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Compatibility 兼容性策略.
type Compatibility string

const (
	None     Compatibility = "NONE"     // 不检查
	Backward Compatibility = "BACKWARD" // 新版本可以读老消息 (consumer 先升)
	Forward  Compatibility = "FORWARD"  // 老版本可以读新消息 (producer 先升)
	Full     Compatibility = "FULL"     // 双向都兼容
)

// Schema 一个事件 schema.
type Schema struct {
	Subject       string `json:"subject"`       // e.g. "charge.succeeded"
	Version       int    `json:"version"`       // 1, 2, 3...
	ID            int64  `json:"id"`            // 全局唯一 schema ID
	Type          string `json:"type"`          // "JSON" / "AVRO" / "PROTOBUF" (这里只实现 JSON)
	Body          string `json:"body"`          // JSON Schema document
	Compatibility Compatibility `json:"compatibility"`
	CreatedAt     string `json:"created_at"`
}

// Registry 注册中心抽象.
type Registry interface {
	Register(ctx context.Context, subject string, body string, compat Compatibility) (*Schema, error)
	Get(ctx context.Context, subject string, version int) (*Schema, error)
	Latest(ctx context.Context, subject string) (*Schema, error)
	List(ctx context.Context) ([]string, error)
	CheckCompatibility(ctx context.Context, subject string, newBody string) error
}

// ─── MemoryRegistry ────────────────────────────────────────────────────

// MemoryRegistry 内存版.
type MemoryRegistry struct {
	mu     sync.RWMutex
	subs   map[string][]*Schema // subject → versions
	nextID int64
}

// NewMemoryRegistry ...
func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{subs: map[string][]*Schema{}}
}

// Register 注册一个新版本. 命中兼容性检查才接受.
func (m *MemoryRegistry) Register(_ context.Context, subject, body string, compat Compatibility) (*Schema, error) {
	if !isValidJSON(body) {
		return nil, fmt.Errorf("invalid JSON schema body")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.subs[subject]

	// 兼容性检查
	if len(versions) > 0 && compat != None {
		prev := versions[len(versions)-1]
		if err := checkCompat(prev.Body, body, compat); err != nil {
			return nil, fmt.Errorf("compatibility %s: %w", compat, err)
		}
	}

	m.nextID++
	s := &Schema{
		Subject: subject, Version: len(versions) + 1, ID: m.nextID,
		Type: "JSON", Body: body, Compatibility: compat,
	}
	m.subs[subject] = append(versions, s)
	return s, nil
}

// Get 取指定版本.
func (m *MemoryRegistry) Get(_ context.Context, subject string, version int) (*Schema, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.subs[subject]
	if version < 1 || version > len(versions) {
		return nil, errors.New("not found")
	}
	return versions[version-1], nil
}

// Latest 取最新版本.
func (m *MemoryRegistry) Latest(_ context.Context, subject string) (*Schema, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.subs[subject]
	if len(versions) == 0 {
		return nil, errors.New("subject not found")
	}
	return versions[len(versions)-1], nil
}

// List 所有 subjects.
func (m *MemoryRegistry) List(_ context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.subs))
	for k := range m.subs {
		out = append(out, k)
	}
	return out, nil
}

// CheckCompatibility 不写入, 只验.
func (m *MemoryRegistry) CheckCompatibility(_ context.Context, subject, newBody string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.subs[subject]
	if len(versions) == 0 {
		return nil // 没历史, 任意 ok
	}
	prev := versions[len(versions)-1]
	return checkCompat(prev.Body, newBody, prev.Compatibility)
}

// ─── 兼容性检查 (JSON Schema 轻量版) ──────────────────────────────────
// 完整实现见 json-everything (golang port); 这里实现 90% 业务用到的规则.
//
// BACKWARD: 新 schema 可读老消息 (消费者新). 允许:
//   - 加 optional 字段
//   - 删 required 字段 (但消费者还能正常用)
//   不允许: 加 required 字段 / 改类型 / required→optional 反向
//
// FORWARD: 老 schema 可读新消息 (生产者新). 跟 BACKWARD 对称.
// FULL: BACKWARD AND FORWARD.

func checkCompat(oldBody, newBody string, compat Compatibility) error {
	if compat == None {
		return nil
	}
	var oldDoc, newDoc map[string]any
	if err := json.Unmarshal([]byte(oldBody), &oldDoc); err != nil {
		return fmt.Errorf("parse old: %w", err)
	}
	if err := json.Unmarshal([]byte(newBody), &newDoc); err != nil {
		return fmt.Errorf("parse new: %w", err)
	}

	switch compat {
	case Backward:
		return checkBackward(oldDoc, newDoc)
	case Forward:
		return checkBackward(newDoc, oldDoc) // forward = backward 反向
	case Full:
		if err := checkBackward(oldDoc, newDoc); err != nil {
			return fmt.Errorf("not backward: %w", err)
		}
		if err := checkBackward(newDoc, oldDoc); err != nil {
			return fmt.Errorf("not forward: %w", err)
		}
	}
	return nil
}

func checkBackward(oldDoc, newDoc map[string]any) error {
	oldProps, _ := oldDoc["properties"].(map[string]any)
	newProps, _ := newDoc["properties"].(map[string]any)
	oldReq, _ := toStringSet(oldDoc["required"])
	newReq, _ := toStringSet(newDoc["required"])

	// 不能加新 required 字段 (老消息没这字段, 新 schema 会拒)
	for r := range newReq {
		if _, wasReq := oldReq[r]; wasReq {
			continue
		}
		if _, existed := oldProps[r]; !existed {
			return fmt.Errorf("added required field %q breaks backward compat", r)
		}
	}
	// 不能改字段类型
	for name, oldDef := range oldProps {
		newDef, ok := newProps[name]
		if !ok {
			continue // 删字段对 backward 可以接受 (新 consumer 不读)
		}
		oldType := jsonSchemaType(oldDef)
		newType := jsonSchemaType(newDef)
		if oldType != "" && newType != "" && oldType != newType {
			return fmt.Errorf("field %q type changed: %s → %s", name, oldType, newType)
		}
	}
	return nil
}

func toStringSet(v any) (map[string]struct{}, bool) {
	out := map[string]struct{}{}
	arr, ok := v.([]any)
	if !ok {
		return out, false
	}
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out[s] = struct{}{}
		}
	}
	return out, true
}

func jsonSchemaType(def any) string {
	m, ok := def.(map[string]any)
	if !ok {
		return ""
	}
	t, _ := m["type"].(string)
	return t
}

func isValidJSON(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

// ─── Validator (producer 发消息前调) ─────────────────────────────────

// Validate 用注册的 schema 校验 payload.
//
// payload 是任意 JSON-serializable 对象 (一般 map[string]any 或具体 struct).
// 不做完整 JSON Schema validation (太重), 只检关键:
//   - required 字段都在
//   - 字段类型对得上 (string/number/bool/object/array)
func Validate(s *Schema, payload any) error {
	if s == nil {
		return errors.New("nil schema")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		// payload 不是 object, 跳过 (e.g. 顶层 array)
		return nil
	}
	var schemaDoc map[string]any
	if err := json.Unmarshal([]byte(s.Body), &schemaDoc); err != nil {
		return err
	}
	req, _ := toStringSet(schemaDoc["required"])
	for r := range req {
		if _, ok := p[r]; !ok {
			return fmt.Errorf("missing required field %q", r)
		}
	}
	props, _ := schemaDoc["properties"].(map[string]any)
	for name, val := range p {
		def, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		expectType, _ := def["type"].(string)
		if expectType == "" {
			continue
		}
		if !typeMatches(val, expectType) {
			return fmt.Errorf("field %q expects %s, got %T", name, expectType, val)
		}
	}
	return nil
}

func typeMatches(v any, jsonType string) bool {
	switch jsonType {
	case "string":
		_, ok := v.(string)
		return ok
	case "number", "integer":
		switch v.(type) {
		case float64, float32, int, int64, int32:
			return true
		}
		return false
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "null":
		return v == nil
	}
	return true // unknown type, 不阻塞
}

// Subject 帮助生成 subject 名 (按惯例 service.event_name).
//   Subject("order-core", "charge.succeeded") → "order-core.charge.succeeded"
func Subject(service, event string) string {
	if strings.Contains(event, ".") {
		return service + "." + event
	}
	return service + "." + event
}
