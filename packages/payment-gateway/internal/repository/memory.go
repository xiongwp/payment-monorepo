package repository

import (
	"context"
	"sync"

	"reconcile-system/packages/payment-gateway/internal/tokenize"
)

// MemoryVault in-mem tokenize.VaultRepo（dev / 测试用）。
type MemoryVault struct {
	mu     sync.RWMutex
	tokens map[string]*tokenize.Token
	pans   map[string][]byte
}

func NewMemoryVault() *MemoryVault {
	return &MemoryVault{
		tokens: map[string]*tokenize.Token{},
		pans:   map[string][]byte{},
	}
}

func (v *MemoryVault) Save(ctx context.Context, t *tokenize.Token, panEnc, cvvEnc []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tokens[t.ID] = t
	v.pans[t.ID] = panEnc
	// CVV (cvvEnc) 故意不存 — PCI 不允许长期存 CVV
	return nil
}

func (v *MemoryVault) Get(ctx context.Context, id string) (*tokenize.Token, []byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	t := v.tokens[id]
	return t, v.pans[id], nil
}

func (v *MemoryVault) MarkUsed(ctx context.Context, id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if t, ok := v.tokens[id]; ok {
		t.Used = true
	}
	return nil
}

// NopKMS 简化 KMS — XOR 一个常量字节（生产换 kms-manage gRPC client）。
type NopKMS struct{ Key byte }

func (k NopKMS) Encrypt(_ context.Context, plain []byte, _ string) ([]byte, error) {
	out := make([]byte, len(plain))
	for i, b := range plain {
		out[i] = b ^ k.Key
	}
	return out, nil
}

func (k NopKMS) Decrypt(_ context.Context, cipher []byte, _ string) ([]byte, error) {
	return k.Encrypt(nil, cipher, "") // XOR 对称
}
