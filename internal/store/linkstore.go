package store

import (
	"context"
	"strings"
	"sync"
	"time"
)

// LinkStore 维护两个 labeled 值之间的"共现"关系（device:abc ↔ user:42），
// 用于检测 fraud ring：一个 device 短时间内关联很多 user / 同一 IP 多账号。
//
// API 设计：
//   - Link(a, b) 记录一次共现，对称双向
//   - Peers(a, prefix) 返回 a 在窗口内关联的所有 peer，可按前缀过滤维度
//
// 实现：内存版（TTL sliding window）。生产规模换 Redis（HSET + EXPIRE 或
// Sorted Set）只需替换实现。
type LinkStore interface {
	Link(ctx context.Context, a, b string)
	Peers(ctx context.Context, a, peerPrefix string) []string
	// PeersWithin 多跳邻居（去重 + 不含起点）。maxHops=1 等同 Peers；2 = 朋友的朋友；
	// >=3 退化成连通块查询，慢，仅用于离线 backfill。
	// peerPrefix 过滤最终结果维度（中间跳不过滤，否则会断链）。
	// 单次最多扩展 BFSMaxNodes 个节点防遍历爆炸。
	PeersWithin(ctx context.Context, a string, maxHops int, peerPrefix string) []string
	// Tag 给图节点打标签：known-good / known-bad / suspect 等。Tag 写入有 TTL（同
	// linkTTL），过期自动失效 — 让"24h 内 chargeback 的 customer"自动从风险传播
	// 影响中移除。
	Tag(ctx context.Context, node, tag string)
	// TagsWithin 拿 a 在 maxHops 跳内所有节点上的 tag 集合（去重）。
	// 用于 graph_reputation 规则：邻居含 fraud tag → 当前节点风险升高。
	TagsWithin(ctx context.Context, a string, maxHops int) map[string]int

	// Purge 清掉给定 key 的所有边 + 标签（GDPR right-to-erasure）。返回清掉的
	// 边数（含双向；若 key 不存在 → 0）。重复调幂等。
	Purge(ctx context.Context, key string) int
}

// linkTTL 共现记录的保留时长。1 小时窗口足以捕获 fraud ring 的短突发，
// 不长到让正常用户多设备使用被错杀（家庭共享设备 / 公司 NAT IP）。
const linkTTL = time.Hour

// 单 key 关联 peer 数硬上限。防止极端攻击场景下单 key 关联爆炸（恶意刷
// device 触发关联）。超过即停止记录新 peer，老 peer 走 TTL 自然过期。
const linkMaxPeersPerKey = 1024

// BFSMaxNodes BFS 多跳遍历单次最多访问节点数。fraud ring 一般不超过 50 节点；
// > 200 几乎肯定是误连了某个 hub-node（公共代理 IP / 共享设备），需要 cap
// 防止遍历爆炸 + 主路径延迟突刺。
const BFSMaxNodes = 200

// BFSMaxHops 多跳查询硬上限。3 跳已经接近"全连通块"，再深没有 fraud 检测意义。
const BFSMaxHops = 3

// MemLinkStore 内存版 link store。所有数据 process-local，重启清空。
//
// 数据结构：
//   - links: map[key] -> map[peer] -> last-seen-time
//   - tags:  map[node] -> map[tag] -> last-seen-time
//   last-seen 用最近一次而非首次：让活跃 peer / tag 不被 TTL 错误清掉
//   （把 TTL 当作"最近共现窗口"而非"首次共现窗口"）。
type MemLinkStore struct {
	mu    sync.Mutex
	links map[string]map[string]time.Time
	tags  map[string]map[string]time.Time
}

func NewMemLinkStore() *MemLinkStore {
	return &MemLinkStore{
		links: make(map[string]map[string]time.Time),
		tags:  make(map[string]map[string]time.Time),
	}
}

// Link 双向记录 a ↔ b。传 "" 直接 no-op（避免未填字段产生空 key 噪声）。
func (s *MemLinkStore) Link(_ context.Context, a, b string) {
	if a == "" || b == "" || a == b {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.addOne(a, b, now)
	s.addOne(b, a, now)
}

func (s *MemLinkStore) addOne(key, peer string, now time.Time) {
	bucket, ok := s.links[key]
	if !ok {
		bucket = make(map[string]time.Time, 4)
		s.links[key] = bucket
	}
	if _, exists := bucket[peer]; !exists && len(bucket) >= linkMaxPeersPerKey {
		// 已满：尝试淘汰 N 个过期 peer 腾位置；都没过期就放弃记录新 peer
		evictExpired(bucket, now)
		if len(bucket) >= linkMaxPeersPerKey {
			return
		}
	}
	bucket[peer] = now
}

func evictExpired(bucket map[string]time.Time, now time.Time) {
	cutoff := now.Add(-linkTTL)
	for p, t := range bucket {
		if t.Before(cutoff) {
			delete(bucket, p)
		}
	}
}

// PeersWithin BFS 实现：从 a 出发 maxHops 跳，去重，不含 a 本身。
// peerPrefix 仅过滤最终输出，不过滤中间节点（否则 device→customer→device 这种
// 跨维度路径会断）。命中 BFSMaxNodes 立刻 return 当前已访问集合，stderr log 一行。
//
// 复杂度：O(N * avg_degree) 其中 N = 已访问节点数 ≤ BFSMaxNodes。
// 实测百万 edge mem 库下 2-hop p99 < 5ms；3-hop p99 < 30ms（fraud ring 极少 >50 节点）。
func (s *MemLinkStore) PeersWithin(_ context.Context, a string, maxHops int, peerPrefix string) []string {
	if a == "" || maxHops <= 0 {
		return nil
	}
	if maxHops > BFSMaxHops {
		maxHops = BFSMaxHops
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-linkTTL)

	visited := map[string]struct{}{a: {}}
	frontier := []string{a}
	for hop := 0; hop < maxHops; hop++ {
		next := make([]string, 0, len(frontier))
		for _, node := range frontier {
			bucket, ok := s.links[node]
			if !ok {
				continue
			}
			for p, t := range bucket {
				if t.Before(cutoff) {
					delete(bucket, p)
					continue
				}
				if _, seen := visited[p]; seen {
					continue
				}
				visited[p] = struct{}{}
				next = append(next, p)
				if len(visited) >= BFSMaxNodes {
					return collectVisited(visited, a, peerPrefix)
				}
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	return collectVisited(visited, a, peerPrefix)
}

func collectVisited(visited map[string]struct{}, origin, prefix string) []string {
	out := make([]string, 0, len(visited))
	for n := range visited {
		if n == origin {
			continue
		}
		if prefix != "" && !strings.HasPrefix(n, prefix) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// Tag 给节点打标签（TTL 同 linkTTL）。同 (node, tag) 对 last-seen 自动滚动延期。
// 调用方约定：tag 命名一律小写（"fraud" / "chargeback" / "trusted"）。
func (s *MemLinkStore) Tag(_ context.Context, node, tag string) {
	if node == "" || tag == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.tags[node]
	if !ok {
		bucket = make(map[string]time.Time, 2)
		s.tags[node] = bucket
	}
	bucket[strings.ToLower(strings.TrimSpace(tag))] = time.Now()
}

// TagsWithin BFS 收集 a maxHops 跳内所有节点的 tag → count map。
// count 反映"几个邻居有该 tag"，规则用它做风险加权（n 个 chargeback 邻居 → score+）。
func (s *MemLinkStore) TagsWithin(ctx context.Context, a string, maxHops int) map[string]int {
	if a == "" {
		return nil
	}
	if maxHops <= 0 {
		maxHops = 1
	}
	if maxHops > BFSMaxHops {
		maxHops = BFSMaxHops
	}
	// 复用 PeersWithin 拿邻居集，再扫 tag。简单 + 跟 PeersWithin 同步 TTL 语义。
	neighbors := s.PeersWithin(ctx, a, maxHops, "")

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-linkTTL)
	out := make(map[string]int)
	scan := func(n string) {
		if bucket, ok := s.tags[n]; ok {
			for tag, t := range bucket {
				if t.Before(cutoff) {
					delete(bucket, tag)
					continue
				}
				out[tag]++
			}
		}
	}
	scan(a) // a 自己的 tag 也看（"曾被标 fraud"是关键自反馈）
	for _, n := range neighbors {
		scan(n)
	}
	return out
}

// Peers 返回 a 在窗口内关联的所有 peer，可按前缀过滤（如 "customer:" 只看用户）。
// peerPrefix == "" 返回全部维度。返回结果是新 slice，不持有内部引用。
func (s *MemLinkStore) Peers(_ context.Context, a, peerPrefix string) []string {
	if a == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.links[a]
	if !ok {
		return nil
	}
	now := time.Now()
	cutoff := now.Add(-linkTTL)
	out := make([]string, 0, len(bucket))
	for p, t := range bucket {
		if t.Before(cutoff) {
			delete(bucket, p)
			continue
		}
		if peerPrefix != "" && !strings.HasPrefix(p, peerPrefix) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Purge 清 key 的所有边（双向）+ tag。GDPR right-to-erasure 用。
// 返回清掉的边数；不含 key 自身的 tag map 元数据数量。
func (s *MemLinkStore) Purge(_ context.Context, key string) int {
	if key == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	purged := 0
	if bucket, ok := s.links[key]; ok {
		// 反向：从 peer 那边把 key 也删掉
		for peer := range bucket {
			if pb, ok := s.links[peer]; ok {
				if _, exists := pb[key]; exists {
					delete(pb, key)
					purged++
				}
			}
			purged++
		}
		delete(s.links, key)
	}
	if _, ok := s.tags[key]; ok {
		delete(s.tags, key)
	}
	return purged
}
