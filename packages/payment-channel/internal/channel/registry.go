package channel

import (
	"fmt"
	"sync"
)

// Registry adapter 注册表。server 层按 adapter 名字分发请求。
type Registry interface {
	Register(a Adapter)
	Get(name string) (Adapter, bool)
	All() map[string]Adapter
	Names() []string
}

type registry struct {
	mu sync.RWMutex
	m  map[string]Adapter
}

func NewRegistry() Registry {
	return &registry{m: make(map[string]Adapter)}
}

func (r *registry) Register(a Adapter) {
	if a == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[a.Name()] = a
}

func (r *registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.m[name]
	return a, ok
}

func (r *registry) All() map[string]Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Adapter, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}

func (r *registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	return out
}

// MustGet 取不到直接 panic（仅用于启动期确认 adapter 已注册）。
func MustGet(r Registry, name string) Adapter {
	a, ok := r.Get(name)
	if !ok {
		panic(fmt.Sprintf("adapter %q not registered", name))
	}
	return a
}
