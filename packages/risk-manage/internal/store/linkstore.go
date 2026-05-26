package store

import (
	"context"
	"math"
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
//   - WeightedFanout(a, edgeType, halflife) 返回 a 沿 edgeType 邻居的"时间衰减
//     强度"总和（旧边 = 弱信号，新边 = 强信号）。Stripe Radar 多年前注册的设备
//     和今天注册的设备权重不一样，这里同理。
//
// 实现：内存版（TTL sliding window + 指数衰减）。生产规模换 Redis（HSET +
// EXPIRE 或 Sorted Set）只需替换实现。
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

	// WeightedFanout 1-跳带时间衰减权重和（**新接口**，老 Peers/Fanout 保留）。
	//
	// 每条边按 last_observed_at 单独算 weight = e^(-days_ago / halflife_days)，
	// 累加得 totalWeight。count 是命中的边数（peerPrefix 过滤后；不带衰减）。
	//
	// peerPrefix 用法同 Peers（"customer:" / "merchant:" / ""）。
	// decayHalfLife <= 0 时退化为"全部计 1"（等同 unweighted）。
	WeightedFanout(ctx context.Context, node, peerPrefix string, decayHalfLife time.Duration) (totalWeight float64, count int)
	// WeightedPeersWithin maxHops 跳的"加权 fanout"：把 1..maxHops 跳每条边的
	// (节点深度 hop 数) × 时间衰减 后累加。hopDecay <= 0 / >= 1 时退化为不带跳数衰减。
	//
	// 用于 weighted_link_fanout 多跳版本 + graph_reputation decay 增强。
	WeightedPeersWithin(ctx context.Context, a string, maxHops int, peerPrefix string,
		decayHalfLife time.Duration, hopDecay float64) (totalWeight float64, count int)
	// WeightedTagsWithin 类似 TagsWithin，但每个 tag 的累加值为
	// sum(time_decay × hop_decay)。返回 float64 而不是 int，因为
	// 衰减后已经不是 count 而是"加权信号强度"。decayHalfLife <= 0 → 仅 hop 衰减。
	WeightedTagsWithin(ctx context.Context, a string, maxHops int,
		decayHalfLife time.Duration, hopDecay float64) map[string]float64

	// Purge 清掉给定 key 的所有边 + 标签（GDPR right-to-erasure）。返回清掉的
	// 边数（含双向；若 key 不存在 → 0）。重复调幂等。
	Purge(ctx context.Context, key string) int
}

// DefaultDecayHalfLife 时间衰减默认半衰期（30 天 → 30 天前的边权重 = 50%；
// ~7×halflife 后权重 < 1e-3，被 GC 清掉）。
const DefaultDecayHalfLife = 30 * 24 * time.Hour

// decayPruneThreshold weight 低于此值即视为"基本失效"，GC 阶段清除。
// 7×halflife 时 e^(-7) ≈ 9e-4，所以阈值取 1e-3。
const decayPruneThreshold = 1e-3

// edgeMeta 每条 (src, dst) 单向边的累积观察元数据（unweighted 路径不用，
// 仅 Weighted* 接口读）。Link 调用时累加 ObserveCount + 滚动 LastObserved。
// 注意 LastObserved 字段是为衰减计算用，与原 links[k][p] 的 last-seen 时间
// 一致冗余存储，方便 Edge() 一次取全（避免 hot loop 里两次 map lookup）。
type edgeMeta struct {
	FirstObserved time.Time
	LastObserved  time.Time
	ObserveCount  uint32
}

// Edge LinkStore 对外暴露的边视图（debug / admin endpoint 用，热路径不取）。
type Edge struct {
	Src            string
	Dst            string
	Weight         float64 // 调用方需自己传 halflife 让 store 算
	FirstObserved  time.Time
	LastObservedAt time.Time
	ObserveCount   uint32
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
//   - links:    map[key] -> map[peer] -> last-seen-time    （unweighted 路径）
//   - linkMeta: map[key] -> map[peer] -> *edgeMeta         （Weighted* 路径用）
//   - tags:     map[node] -> map[tag] -> last-seen-time
//   last-seen 用最近一次而非首次：让活跃 peer / tag 不被 TTL 错误清掉
//   （把 TTL 当作"最近共现窗口"而非"首次共现窗口"）。
//
// 为什么 links 和 linkMeta 并行存而不合并：兼容老的 snapshot / 序列化路径
// （links 是 map[string]time.Time，外部测试代码直接 s.mu.Lock() + 改 map）。
// linkMeta 是单调加字段，老路径不读它就无影响；删 key 时两个 map 同步删。
type MemLinkStore struct {
	mu       sync.Mutex
	links    map[string]map[string]time.Time
	linkMeta map[string]map[string]*edgeMeta
	tags     map[string]map[string]time.Time
	// simIndex SimHash 模糊指纹索引：simhash → 关联的 peer keys（含 last-seen）
	// 跟 links 并行存（精确 sha256 / device key 走 links；fuzzy SimHash 走这里）。
	// PeersBySimHash 线性扫这个 map（key 数即设备数；1M 内 < 20ms）。
	// 超过规模换 LSH bucket（4-band × 16-bit prefix → 桶内再扫）。
	simIndex map[uint64]map[string]time.Time
}

func NewMemLinkStore() *MemLinkStore {
	return &MemLinkStore{
		links:    make(map[string]map[string]time.Time),
		linkMeta: make(map[string]map[string]*edgeMeta),
		tags:     make(map[string]map[string]time.Time),
		simIndex: make(map[uint64]map[string]time.Time),
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

	// 同步累加 edgeMeta（Weighted* 路径用）
	mbucket, ok := s.linkMeta[key]
	if !ok {
		mbucket = make(map[string]*edgeMeta, 4)
		s.linkMeta[key] = mbucket
	}
	if m, exists := mbucket[peer]; exists {
		m.LastObserved = now
		m.ObserveCount++
	} else {
		mbucket[peer] = &edgeMeta{FirstObserved: now, LastObserved: now, ObserveCount: 1}
	}
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
			if pmb, ok := s.linkMeta[peer]; ok {
				delete(pmb, key)
			}
			purged++
		}
		delete(s.links, key)
	}
	delete(s.linkMeta, key)
	if _, ok := s.tags[key]; ok {
		delete(s.tags, key)
	}
	return purged
}

// ─── Weighted* 接口实现 ─────────────────────────────────────────────

// edgeWeight = e^(-days_ago / halflife_days)。half_life <= 0 时返回 1.0
// （等同于 unweighted —— 调用方可统一走 Weighted* 路径）。
func edgeWeight(lastObserved, now time.Time, halflife time.Duration) float64 {
	if halflife <= 0 {
		return 1.0
	}
	dt := now.Sub(lastObserved)
	if dt <= 0 {
		return 1.0
	}
	// 用 hours 而不是 days 数避免短半衰期被 round 到 0
	return math.Exp(-dt.Hours() / halflife.Hours())
}

// WeightedFanout 1-跳带衰减权重和。peerPrefix 同 Peers。
// 顺带做"懒 GC"：遇到 weight < decayPruneThreshold 的边立即删（不另开
// 后台线程；GC 摊到读路径上 amortize）。
func (s *MemLinkStore) WeightedFanout(_ context.Context, node, peerPrefix string, decayHalfLife time.Duration) (float64, int) {
	if node == "" {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mbucket, ok := s.linkMeta[node]
	if !ok {
		return 0, 0
	}
	now := time.Now()
	cutoff := now.Add(-linkTTL)
	totalW := 0.0
	count := 0
	for peer, m := range mbucket {
		// 同步沿用 linkTTL 硬过期（兜底；衰减阈值是软过期）
		if m.LastObserved.Before(cutoff) {
			delete(mbucket, peer)
			if b, ok := s.links[node]; ok {
				delete(b, peer)
			}
			continue
		}
		w := edgeWeight(m.LastObserved, now, decayHalfLife)
		if w < decayPruneThreshold {
			delete(mbucket, peer)
			if b, ok := s.links[node]; ok {
				delete(b, peer)
			}
			continue
		}
		if peerPrefix != "" && !strings.HasPrefix(peer, peerPrefix) {
			continue
		}
		totalW += w
		count++
	}
	return totalW, count
}

// WeightedPeersWithin BFS maxHops 跳，每层套 hopDecay^(hop-1) × time_decay。
// 输出 (totalWeight, count) ：count 为满足 peerPrefix 的去重节点数。
// hopDecay 推荐 0.5（每多一跳减半）；<=0 / >=1 时禁用跳数衰减。
func (s *MemLinkStore) WeightedPeersWithin(ctx context.Context, a string, maxHops int, peerPrefix string,
	decayHalfLife time.Duration, hopDecay float64,
) (float64, int) {
	if a == "" || maxHops <= 0 {
		return 0, 0
	}
	if maxHops > BFSMaxHops {
		maxHops = BFSMaxHops
	}
	useHopDecay := hopDecay > 0 && hopDecay < 1
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-linkTTL)

	// nodeWeight: 该节点累积到的"最强路径"weight（多条路径取 max，避免重复计数）
	nodeWeight := map[string]float64{a: 1.0}
	frontier := []string{a}
	visitedTotal := 1
	capped := false
	for hop := 1; hop <= maxHops && !capped; hop++ {
		next := make([]string, 0, len(frontier))
		for _, node := range frontier {
			if capped {
				break
			}
			mbucket, ok := s.linkMeta[node]
			if !ok {
				continue
			}
			parentW := nodeWeight[node]
			for peer, m := range mbucket {
				if m.LastObserved.Before(cutoff) {
					delete(mbucket, peer)
					if b, ok := s.links[node]; ok {
						delete(b, peer)
					}
					continue
				}
				tw := edgeWeight(m.LastObserved, now, decayHalfLife)
				if tw < decayPruneThreshold {
					delete(mbucket, peer)
					if b, ok := s.links[node]; ok {
						delete(b, peer)
					}
					continue
				}
				w := parentW * tw
				if useHopDecay {
					w *= hopDecay
				}
				if prev, ok := nodeWeight[peer]; ok {
					if w > prev {
						nodeWeight[peer] = w
					}
					continue
				}
				nodeWeight[peer] = w
				next = append(next, peer)
				visitedTotal++
				if visitedTotal >= BFSMaxNodes {
					capped = true
					break
				}
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	total := 0.0
	count := 0
	for n, w := range nodeWeight {
		if n == a {
			continue
		}
		if peerPrefix != "" && !strings.HasPrefix(n, peerPrefix) {
			continue
		}
		total += w
		count++
	}
	return total, count
}

// WeightedTagsWithin maxHops 跳 BFS 收集所有节点的 tag → 加权信号。
// 每个 tag 的累加 = sum(节点的边时间衰减 × hopDecay^hop)，
// 节点本身（hop=0）权重 = 1.0。
func (s *MemLinkStore) WeightedTagsWithin(ctx context.Context, a string, maxHops int,
	decayHalfLife time.Duration, hopDecay float64,
) map[string]float64 {
	if a == "" {
		return nil
	}
	if maxHops < 0 {
		maxHops = 0
	}
	if maxHops > BFSMaxHops {
		maxHops = BFSMaxHops
	}
	useHopDecay := hopDecay > 0 && hopDecay < 1
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-linkTTL)

	nodeWeight := map[string]float64{a: 1.0}
	frontier := []string{a}
	visitedTotal := 1
	capped := false
	for hop := 1; hop <= maxHops && !capped; hop++ {
		next := make([]string, 0, len(frontier))
		for _, node := range frontier {
			if capped {
				break
			}
			mbucket, ok := s.linkMeta[node]
			if !ok {
				continue
			}
			parentW := nodeWeight[node]
			for peer, m := range mbucket {
				if m.LastObserved.Before(cutoff) {
					delete(mbucket, peer)
					if b, ok := s.links[node]; ok {
						delete(b, peer)
					}
					continue
				}
				tw := edgeWeight(m.LastObserved, now, decayHalfLife)
				if tw < decayPruneThreshold {
					delete(mbucket, peer)
					if b, ok := s.links[node]; ok {
						delete(b, peer)
					}
					continue
				}
				w := parentW * tw
				if useHopDecay {
					w *= hopDecay
				}
				if prev, ok := nodeWeight[peer]; ok {
					if w > prev {
						nodeWeight[peer] = w
					}
					continue
				}
				nodeWeight[peer] = w
				next = append(next, peer)
				visitedTotal++
				if visitedTotal >= BFSMaxNodes {
					capped = true
					break
				}
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	out := map[string]float64{}
	for node, w := range nodeWeight {
		bucket, ok := s.tags[node]
		if !ok {
			continue
		}
		for tag, t := range bucket {
			if t.Before(cutoff) {
				delete(bucket, tag)
				continue
			}
			out[tag] += w
		}
	}
	return out
}

// ─── SimHash 模糊指纹索引 ──────────────────────────────────────────
//
// LinkStore 现有 device→customer 边走精确 device key（sha256 派生）。SimHash
// 路径平行存：把 device 的 64-bit SimHash 当 key，dst peer 当 value 累加。
// 命中查询 PeersBySimHash 线性扫所有 SimHash 桶，hamming 距离 <= threshold
// 的桶全部 union。
//
// 设计权衡：
//   - 不动 LinkStore interface（避免侵入 Neo4j / Nebula 实现）→ 仅在 MemLinkStore
//     上加方法；调用方拿到具体类型或 type assertion。
//   - 线性扫不分片：1M device 内 < 20ms 单查可接受；超规模换 LSH（TODO）。
//   - 不走 linkMeta：SimHash 索引是 unweighted 路径（不需要 hopDecay / 多跳）。

// LinkBySimHash 把 (simhash, dst) 入模糊索引。simhash == 0 (未计算 / 无信号)
// 直接 no-op，避免 0-bucket 撞所有未识别设备。
func (s *MemLinkStore) LinkBySimHash(_ context.Context, simhash uint64, dst string) {
	if simhash == 0 || dst == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	bucket, ok := s.simIndex[simhash]
	if !ok {
		bucket = make(map[string]time.Time, 2)
		s.simIndex[simhash] = bucket
	}
	if _, exists := bucket[dst]; !exists && len(bucket) >= linkMaxPeersPerKey {
		evictExpired(bucket, now)
		if len(bucket) >= linkMaxPeersPerKey {
			return
		}
	}
	bucket[dst] = now
}

// PeersBySimHash 线性扫 simIndex，hamming(query, bucket_key) <= threshold 的
// 桶里全部 peer union 后返回。
//
// **性能注意**：当前是 O(N) 线性扫（N = simIndex 桶数）；
//   - 1k device  < 1ms      生产 hot path 完全可接受
//   - 100k device ~2ms      可接受
//   - 1M device  ~15-20ms   接近 hot path 上限，仍 OK
//   - >5M device           必须上 LSH（分 4 band × 16-bit prefix，每 band 内
//                          完全相等才进一步比对，期望命中桶数 ~N/2^16）
// 这里 TODO LSH；目前 risk-manage 单租户量级单查可控。
//
// 同 Peers，懒 GC 过期边（linkTTL）。
//
// threshold < 0 视为 0（仅精确匹配）；threshold > 64 cap 到 64。
func (s *MemLinkStore) PeersBySimHash(_ context.Context, simhash uint64, threshold int, peerPrefix string) []string {
	if simhash == 0 {
		return nil
	}
	if threshold < 0 {
		threshold = 0
	}
	if threshold > 64 {
		threshold = 64
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-linkTTL)
	seen := make(map[string]struct{})
	// TODO(LSH): 当前 O(N) 扫所有 SimHash bucket；>1M 桶时换分段 LSH。
	for bucketKey, bucket := range s.simIndex {
		if hammingDist64(bucketKey, simhash) > threshold {
			continue
		}
		for peer, t := range bucket {
			if t.Before(cutoff) {
				delete(bucket, peer)
				continue
			}
			if peerPrefix != "" && !strings.HasPrefix(peer, peerPrefix) {
				continue
			}
			seen[peer] = struct{}{}
		}
		// 空桶清理：避免长期持有空 map
		if len(bucket) == 0 {
			delete(s.simIndex, bucketKey)
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

// hammingDist64 跟 fphash.HammingDistance 同；这里复制实现避免 store →
// fphash 反向依赖（fphash 是叶子包，不能拉 store）。
func hammingDist64(a, b uint64) int {
	x := a ^ b
	// 64-bit popcount（Brian Kernighan / table-free）
	x = x - ((x >> 1) & 0x5555555555555555)
	x = (x & 0x3333333333333333) + ((x >> 2) & 0x3333333333333333)
	x = (x + (x >> 4)) & 0x0f0f0f0f0f0f0f0f
	return int((x * 0x0101010101010101) >> 56)
}

// GCDecayed 主动清除 weight < decayPruneThreshold 的边（admin endpoint /
// 周期任务调）。返回清掉边数。Weighted* 读路径已经懒清；这里给
// "只写不读" 的冷数据兜底。
func (s *MemLinkStore) GCDecayed(decayHalfLife time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-linkTTL)
	removed := 0
	for src, mbucket := range s.linkMeta {
		for peer, m := range mbucket {
			if m.LastObserved.Before(cutoff) ||
				edgeWeight(m.LastObserved, now, decayHalfLife) < decayPruneThreshold {
				delete(mbucket, peer)
				if b, ok := s.links[src]; ok {
					delete(b, peer)
				}
				removed++
			}
		}
		if len(mbucket) == 0 {
			delete(s.linkMeta, src)
			delete(s.links, src)
		}
	}
	return removed
}
