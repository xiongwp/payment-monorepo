// Package service 实现 KMSService 的业务逻辑 —— 纯 Go，不依赖 gRPC。
package service

import (
	"container/list"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/kms-manage/internal/cryptoenv"
	"github.com/xiongwp/kms-manage/internal/keystore"
	"github.com/xiongwp/kms-manage/internal/metrics"
)

// ErrKeyNotFound 指定的 key_id 不在 keystore 里。
var ErrKeyNotFound = errors.New("kms: key_id not found")

// decryptCacheTTL 解密结果缓存 TTL。
//
// 命中场景：业务服务启动期 secret.Resolve 为每个 `kms:v1:` 字段调一次 Decrypt；
// 同一字段（同一密文）在多副本启动 / 配置 reload / 周期 sync 时被反复解密。
// 5min 让启动 burst 和短期 reload 击中缓存，又不会让 key rotation 慢得离谱
// （rotation 后 5min 内残留命中——但反正 rotation 不删旧 key，只是切 active）。
const decryptCacheTTL = 5 * time.Minute

// decryptCacheMaxEntries 缓存最大条目数。一个业务服务通常只有几十条 yaml 密文字段，
// 整套生态系统也就几百条；上限 4096 远远够用，超过即触发 LRU 驱逐。
const decryptCacheMaxEntries = 4096

type decryptCacheEntry struct {
	plaintext []byte
	keyID     string
	expires   time.Time
}

// lruNode 双向链表节点 payload。
type lruNode struct {
	key   string
	entry decryptCacheEntry
}

// decryptCache 是 KMS 解密结果的 LRU 缓存。
//
// 实现：map[key]*list.Element + container/list 双向链表。
//   - get 命中：把节点移到 front（最近使用）；O(1)
//   - set：相同 key 覆盖 + 移 front；新 key 时若已满，从 back 驱逐 LRU；O(1)
//   - evictExpired：周期 gcLoop 扫整链清过期节点；O(N)
//
// 为何 Mutex 而非 RWMutex：get 也要 MoveToFront 写链表；用 RLock 不安全。
// 此规模（≤ 4096 entry，几十-几百 RPS）写锁无瓶颈。
type decryptCache struct {
	mu       sync.Mutex
	index    map[string]*list.Element // key → list node
	order    *list.List               // front = most recent，back = LRU 候选
	stopOnce sync.Once
	stopCh   chan struct{}
}

func newDecryptCache() *decryptCache {
	c := &decryptCache{
		index:  make(map[string]*list.Element, 64),
		order:  list.New(),
		stopCh: make(chan struct{}),
	}
	go c.gcLoop()
	return c
}

func (c *decryptCache) gcLoop() {
	t := time.NewTicker(decryptCacheTTL)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			c.evictExpired()
		}
	}
}

func (c *decryptCache) evictExpired() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	// 从 back 起扫：连续过期的节点直接 pop（旧节点更可能过期）；遇到未过期
	// 节点不能 break，因为 expires 不严格按 LRU 顺序排（不同 entry 设置时
	// TTL 相同但加入时间不同；而 MoveToFront 只反映 access，不反映 set）。
	for e := c.order.Back(); e != nil; {
		prev := e.Prev()
		n := e.Value.(*lruNode)
		if now.After(n.entry.expires) {
			c.order.Remove(e)
			delete(c.index, n.key)
		}
		e = prev
	}
}

func (c *decryptCache) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

// get 命中且未过期返回 entry，否则 ok=false。命中时把节点移到 front。
func (c *decryptCache) get(key string) (decryptCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return decryptCacheEntry{}, false
	}
	n := el.Value.(*lruNode)
	if time.Now().After(n.entry.expires) {
		// 过期节点遇上即清理；省一次 gcLoop。
		c.order.Remove(el)
		delete(c.index, key)
		return decryptCacheEntry{}, false
	}
	c.order.MoveToFront(el)
	return n.entry, true
}

func (c *decryptCache) set(key string, e decryptCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		// 已存在 → 覆盖值 + 提到 front
		n := el.Value.(*lruNode)
		n.entry = e
		c.order.MoveToFront(el)
		return
	}
	if c.order.Len() >= decryptCacheMaxEntries {
		// 满 → 驱逐最久未使用（list back）
		back := c.order.Back()
		if back != nil {
			c.order.Remove(back)
			delete(c.index, back.Value.(*lruNode).key)
		}
	}
	el := c.order.PushFront(&lruNode{key: key, entry: e})
	c.index[key] = el
}

// clear 清空缓存。key rotation / active key change 时调用，
// 防止旧 active key 下生成的解密结果残留误用。
func (c *decryptCache) clear() {
	c.mu.Lock()
	c.index = make(map[string]*list.Element, 64)
	c.order = list.New()
	c.mu.Unlock()
}

// decryptCacheKey 把 (ciphertext, context) 折成单 string key。
// 用 \x00 分隔——ciphertext 是 ASCII（kms:v1:base64...），context 任意文本，
// 任何使用 NUL 字节的 context 注入也只能制造命中冲突（极不可能且影响有限）。
func decryptCacheKey(ciphertext, ctxStr string) string {
	return ciphertext + "\x00" + ctxStr
}

type KMSService struct {
	store        *keystore.Store
	logger       *zap.Logger
	decryptCache *decryptCache
	// activeKeyMu 保护 activeKeySnapshot；ActiveKeyID() 变化时清缓存。
	activeKeyMu        sync.Mutex
	activeKeySnapshot  string
}

func NewKMSService(store *keystore.Store, logger *zap.Logger) *KMSService {
	return &KMSService{
		store:             store,
		logger:            logger,
		decryptCache:      newDecryptCache(),
		activeKeySnapshot: store.ActiveKeyID(),
	}
}

// Close 释放后台资源（gc goroutine）。fx OnStop 调用。
func (s *KMSService) Close() {
	if s.decryptCache != nil {
		s.decryptCache.Stop()
	}
}

// invalidateCacheIfActiveKeyChanged 检测 active key 切换，发生时清空 decrypt cache。
// 在 Decrypt 入口调用一次即可（每次 RPC 一个原子比较；变化频率极低）。
func (s *KMSService) invalidateCacheIfActiveKeyChanged() {
	cur := s.store.ActiveKeyID()
	s.activeKeyMu.Lock()
	if cur != s.activeKeySnapshot {
		s.activeKeySnapshot = cur
		s.activeKeyMu.Unlock()
		s.decryptCache.clear()
		if s.logger != nil {
			s.logger.Info("kms decrypt cache cleared due to active key change",
				zap.String("new_active_key", cur))
		}
		return
	}
	s.activeKeyMu.Unlock()
}

// EncryptIn/Out：gRPC server 把 proto 翻成这套 DTO，业务纯 Go。
type EncryptIn struct {
	KeyID     string
	Plaintext []byte
	Context   string
}
type EncryptOut struct {
	Ciphertext string
	KeyID      string
}

// Encrypt 没传 key_id 就用 active。
func (s *KMSService) Encrypt(_ context.Context, in EncryptIn) (*EncryptOut, error) {
	kid := in.KeyID
	if kid == "" {
		kid = s.store.ActiveKeyID()
	}
	key, ok := s.store.KeyByID(kid)
	if !ok {
		metrics.KMSOpTotal.WithLabelValues("encrypt", "no_key").Inc()
		return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, kid)
	}
	ct, err := cryptoenv.Encrypt(key, kid, in.Context, in.Plaintext)
	if err != nil {
		metrics.KMSOpTotal.WithLabelValues("encrypt", "err").Inc()
		return nil, err
	}
	metrics.KMSOpTotal.WithLabelValues("encrypt", "ok").Inc()
	return &EncryptOut{Ciphertext: ct, KeyID: kid}, nil
}

type DecryptIn struct {
	Ciphertext string
	Context    string
}
type DecryptOut struct {
	Plaintext []byte
	KeyID     string
}

// Decrypt 会把密文里带的 key_id 查 keystore；找不到就失败。
//
// 短 TTL 缓存命中时跳过 AEAD 解密：业务服务启动期 secret.Resolve 反复解密同一字段
// 是命中 hot spot，缓存把这种 startup burst 摊到一次 cryptoenv.Decrypt。
// 失败结果不缓存（错误路径不应被反复"快速失败"，否则掩盖偶发问题）。
func (s *KMSService) Decrypt(_ context.Context, in DecryptIn) (*DecryptOut, error) {
	s.invalidateCacheIfActiveKeyChanged()

	cacheKey := decryptCacheKey(in.Ciphertext, in.Context)
	if e, ok := s.decryptCache.get(cacheKey); ok {
		metrics.KMSOpTotal.WithLabelValues("decrypt", "cache_hit").Inc()
		// 拷贝 plaintext 防 caller mutate 污染缓存条目
		out := make([]byte, len(e.plaintext))
		copy(out, e.plaintext)
		return &DecryptOut{Plaintext: out, KeyID: e.keyID}, nil
	}

	plain, kid, err := cryptoenv.Decrypt(s.store.Snapshot(), in.Ciphertext, in.Context)
	if err != nil {
		if kid != "" {
			metrics.KMSOpTotal.WithLabelValues("decrypt", "err").Inc()
		} else {
			metrics.KMSOpTotal.WithLabelValues("decrypt", "bad_format").Inc()
		}
		return nil, err
	}
	// 缓存条目存的是真实计算结果的副本（plain 来自 cryptoenv，归属调用方；
	// 我们把另一份独立 slice 放进缓存，二者互不影响）
	cached := make([]byte, len(plain))
	copy(cached, plain)
	s.decryptCache.set(cacheKey, decryptCacheEntry{
		plaintext: cached,
		keyID:     kid,
		expires:   time.Now().Add(decryptCacheTTL),
	})
	metrics.KMSOpTotal.WithLabelValues("decrypt", "ok").Inc()
	return &DecryptOut{Plaintext: plain, KeyID: kid}, nil
}

type GenerateDataKeyIn struct {
	KeyID   string
	Context string
	Bytes   int
}
type GenerateDataKeyOut struct {
	Plaintext    []byte
	Encrypted    string
	KeyID        string
}

// GenerateDataKey 产一个随机 DEK，并用 master key 加密返回。
// 调用方拿到明文 DEK 做本地字段加密后应该立即清零。
func (s *KMSService) GenerateDataKey(_ context.Context, in GenerateDataKeyIn) (*GenerateDataKeyOut, error) {
	n := in.Bytes
	if n == 0 {
		n = 32
	}
	switch n {
	case 16, 24, 32:
	default:
		return nil, fmt.Errorf("kms: dek bytes must be 16/24/32, got %d", n)
	}

	kid := in.KeyID
	if kid == "" {
		kid = s.store.ActiveKeyID()
	}
	master, ok := s.store.KeyByID(kid)
	if !ok {
		metrics.KMSOpTotal.WithLabelValues("gen_dek", "no_key").Inc()
		return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, kid)
	}

	dek := make([]byte, n)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	encDek, err := cryptoenv.Encrypt(master, kid, in.Context, dek)
	if err != nil {
		metrics.KMSOpTotal.WithLabelValues("gen_dek", "err").Inc()
		return nil, err
	}
	metrics.KMSOpTotal.WithLabelValues("gen_dek", "ok").Inc()
	return &GenerateDataKeyOut{
		Plaintext: dek,
		Encrypted: encDek,
		KeyID:     kid,
	}, nil
}

// DescribeKey / ListKeys 用于管理面。

func (s *KMSService) DescribeKey(id string) (keystore.KeyMeta, bool) {
	return s.store.Meta(id)
}

func (s *KMSService) ListKeys() ([]keystore.KeyMeta, string) {
	return s.store.List(), s.store.ActiveKeyID()
}
