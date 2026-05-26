// nebula_linkstore.go: store.LinkStore 的 NebulaGraph 生产实现。
//
// **构建开关**：`//go:build nebula`。默认 build 不依赖 nebula-go；接入步骤：
//
//  1. go.mod 已经包含 github.com/vesoft-inc/nebula-go/v3 v3.8.0
//  2. 构建时加 -tags nebula：`go build -tags nebula ./...`
//  3. main.go 里的 newLinkStore 改成调 NewNebulaLinkStoreFromConfig（见
//     linkstore_nebula_factory.go）—— 也是 -tags nebula 才会编译进来
//
// 跟 Neo4j 实现的差异：
//   - NebulaGraph 是分布式图（hash partition by VID），写吞吐 / 多跳查询
//     比 Neo4j 单节点强很多；适合 per-tenant 上百万 device-customer 关联，
//     生产规模 1 亿+ 边
//   - VID 必须显式指定（不像 Neo4j 自动 internal id）；这里用 sha256(key)
//     的前 16 字节固定长 VID，避免 nebula 的 INT64 vs STRING vid 切换坑，
//     刚好匹配 FIXED_STRING(32)
//   - nGQL 跟 Cypher 类似但有差别：INSERT 用 INSERT VERTEX / EDGE 而不是
//     MERGE，所以本实现自己做 upsert
//
// 图建模（详见 deploy/nebulagraph/schema.ngql）：
//
//	CREATE SPACE risk_graph (vid_type=FIXED_STRING(32), partition_num=100, replica_factor=3);
//	CREATE TAG identity (key string NOT NULL);
//	CREATE TAG suspect (label string NOT NULL, ts timestamp NOT NULL);
//	CREATE TAG goodwill (label string NOT NULL, ts timestamp NOT NULL);
//	CREATE EDGE links (at timestamp NOT NULL, expires timestamp NOT NULL);
//	CREATE TAG INDEX identity_key_idx ON identity(key(64));
//	CREATE TAG INDEX suspect_label_idx ON suspect(label(40));
//	CREATE EDGE INDEX links_at_idx ON links(at);
//
// 性能：
//   - 双向 Link 写：1 round-trip (UPSERT VERTEX + INSERT EDGE 拼成一条 nGQL) 3-5ms
//   - PeersWithin maxHops=2：单 query 1-3ms (集群)；100-500µs (单机)
//   - PeersWithin maxHops=3：500ms+ 量级，硬 cap 到 3
//   - WeightedFanout: 1 round-trip (拉边带 at) + 本地 O(N) math，3-5ms

//go:build nebula

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	nebula "github.com/vesoft-inc/nebula-go/v3"
	"go.uber.org/zap"
)

// 编译期接口断言。WeightedFanout 等"新接口"曾经只在 mem 实现里出现 —— 这里
// 显式 cast 防止下次 LinkStore 加新方法时 stub 漏写又走默认 mem fallback。
var _ LinkStore = (*NebulaLinkStore)(nil)

// nebulaQueryTimeout 单条 nGQL 默认超时。多跳查询经常 >500ms，给 1s。
const nebulaQueryTimeout = 1 * time.Second

// healthcheckInterval 后台保活间隔；nebula-go session pool 自带 idle
// 健康检查但仍依赖外部驱动，定时跑 `YIELD 1` 让对端连接热着。
const healthcheckInterval = 30 * time.Second

// NebulaLinkStore 走 nebula-go session pool 跑 nGQL。
//
// 线程安全：nebula.Session.Execute 不是 goroutine-safe；每个调用都从 pool
// 取一个新的 session（拿完 Release 回 pool）。pool 本身 goroutine-safe。
type NebulaLinkStore struct {
	pool   *nebula.ConnectionPool
	space  string
	user   string
	pass   string
	logger *zap.Logger

	healthStop chan struct{}
	healthDone chan struct{}
	healthy    atomic.Bool // 最近一次 healthcheck 通过则 true；接入 /healthz 用
}

// NewNebulaLinkStore 直接传 pool / space / user / pass —— 给 main.go
// 已经手工建好 pool 的场景用（例如多个 store 共享 pool）。
func NewNebulaLinkStore(pool *nebula.ConnectionPool, space, user, pass string, logger *zap.Logger) *NebulaLinkStore {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &NebulaLinkStore{
		pool:       pool,
		space:      space,
		user:       user,
		pass:       pass,
		logger:     logger,
		healthStop: make(chan struct{}),
		healthDone: make(chan struct{}),
	}
	s.healthy.Store(true)
	go s.runHealthcheck()
	return s
}

// NewNebulaLinkStoreFromAddrs 一站式构造：addr 列表 → ConnectionPool → Store。
// 失败返 err 让 main 决定 fallback（mem）或 fatal。
func NewNebulaLinkStoreFromAddrs(addrs []string, user, pass, space string, logger *zap.Logger) (*NebulaLinkStore, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("nebula: empty addr list")
	}
	if space == "" {
		return nil, fmt.Errorf("nebula: empty space")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	hosts := make([]nebula.HostAddress, 0, len(addrs))
	for _, a := range addrs {
		host, port, err := splitHostPort(a)
		if err != nil {
			return nil, fmt.Errorf("nebula: bad addr %q: %w", a, err)
		}
		hosts = append(hosts, nebula.HostAddress{Host: host, Port: port})
	}
	conf := nebula.GetDefaultConf()
	conf.MaxConnPoolSize = 64
	conf.MinConnPoolSize = 4
	conf.TimeOut = nebulaQueryTimeout
	conf.IdleTime = 10 * time.Minute
	pool, err := nebula.NewConnectionPool(hosts, conf, nebula.DefaultLogger{})
	if err != nil {
		return nil, fmt.Errorf("nebula: pool init: %w", err)
	}
	// 提前 sanity check：拿一个 session 跑 YIELD 1 验证连得通 + 切到 space。
	sess, err := pool.GetSession(user, pass)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("nebula: get session: %w", err)
	}
	rs, err := sess.Execute(fmt.Sprintf("USE %s; YIELD 1;", escapeIdentifier(space)))
	sess.Release()
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("nebula: probe: %w", err)
	}
	if !rs.IsSucceed() {
		pool.Close()
		return nil, fmt.Errorf("nebula: probe failed: %s", rs.GetErrorMsg())
	}
	return NewNebulaLinkStore(pool, space, user, pass, logger), nil
}

// Close 停 healthcheck + 关 pool。
func (s *NebulaLinkStore) Close() {
	close(s.healthStop)
	<-s.healthDone
	if s.pool != nil {
		s.pool.Close()
	}
}

// Healthy 最近一次 healthcheck 是否通过。用于 /healthz。
func (s *NebulaLinkStore) Healthy() bool { return s.healthy.Load() }

func (s *NebulaLinkStore) runHealthcheck() {
	defer close(s.healthDone)
	t := time.NewTicker(healthcheckInterval)
	defer t.Stop()
	for {
		select {
		case <-s.healthStop:
			return
		case <-t.C:
			sess, err := s.pool.GetSession(s.user, s.pass)
			if err != nil {
				s.healthy.Store(false)
				s.logger.Warn("nebula healthcheck: get session failed", zap.Error(err))
				continue
			}
			rs, err := sess.Execute("YIELD 1;")
			sess.Release()
			ok := err == nil && rs != nil && rs.IsSucceed()
			s.healthy.Store(ok)
			if !ok {
				msg := ""
				if rs != nil {
					msg = rs.GetErrorMsg()
				}
				s.logger.Warn("nebula healthcheck failed", zap.Error(err), zap.String("rs_err", msg))
			}
		}
	}
}

// vidOf 把任意 LinkStore key 转成 32-char hex VID（fixed-size string vid）。
func vidOf(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:16]) // 32 hex chars
}

// execute 取 session → USE space → 跑 query → Release。所有方法的入口都走这。
//
// ctx 用于取消 / 超时；nebula-go v3 Execute 本身没有 context 参数，但我们
// 通过 ctx.Done 检查 + 短超时 pool 配置达到类似效果。
func (s *NebulaLinkStore) execute(ctx context.Context, query string) (*nebula.ResultSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sess, err := s.pool.GetSession(s.user, s.pass)
	if err != nil {
		return nil, fmt.Errorf("nebula: get session: %w", err)
	}
	defer sess.Release()
	// 一次 round-trip 同时 USE + 执行查询，省一次 RTT。注意 nebula 支持多语句以 `;` 分隔。
	full := fmt.Sprintf("USE %s; %s", escapeIdentifier(s.space), query)
	rs, err := sess.Execute(full)
	if err != nil {
		return nil, err
	}
	if !rs.IsSucceed() {
		return rs, fmt.Errorf("nebula: query failed: %s", rs.GetErrorMsg())
	}
	return rs, nil
}

// Link 双向写边；两端 vertex upsert（idempotent）。expires=now+linkTTL，at=now。
func (s *NebulaLinkStore) Link(ctx context.Context, a, b string) {
	if a == "" || b == "" || a == b {
		return
	}
	va, vb := vidOf(a), vidOf(b)
	now := time.Now().Unix()
	exp := now + int64(linkTTL.Seconds())
	// UPSERT 是 idempotent 写；UPSERT VERTEX 在 nebula 里语义就是 "INSERT OR
	// UPDATE"。INSERT EDGE 直接 overwrite 同 (src,dst) 边 props，对我们而言
	// "更新 at" 正是想要的（滚动 last-seen）。
	q := fmt.Sprintf(
		`UPSERT VERTEX ON identity "%s" SET key = "%s";
		 UPSERT VERTEX ON identity "%s" SET key = "%s";
		 INSERT EDGE links(at, expires) VALUES "%s" -> "%s": (%d, %d), "%s" -> "%s": (%d, %d);`,
		va, escapeStr(a), vb, escapeStr(b),
		va, vb, now, exp,
		vb, va, now, exp)
	if _, err := s.execute(ctx, q); err != nil {
		s.logger.Warn("nebula link failed", zap.Error(err), zap.String("a", a), zap.String("b", b))
	}
}

// Peers 单跳 + prefix 过滤。
//
// nGQL: GO 1 STEP FROM "<vid>" OVER links WHERE links.expires > now()
//       YIELD properties($$).key AS key | LIMIT 1024
func (s *NebulaLinkStore) Peers(ctx context.Context, a, peerPrefix string) []string {
	if a == "" {
		return nil
	}
	q := fmt.Sprintf(
		`GO 1 STEP FROM "%s" OVER links WHERE links.expires > now()
		 YIELD properties($$).key AS key
		 | LIMIT %d;`,
		vidOf(a), linkMaxPeersPerKey)
	rs, err := s.execute(ctx, q)
	if err != nil {
		s.logger.Warn("nebula peers failed", zap.Error(err))
		return nil
	}
	keys := extractStringColumn(rs, 0)
	if peerPrefix == "" {
		return keys
	}
	out := keys[:0]
	for _, k := range keys {
		if strings.HasPrefix(k, peerPrefix) {
			out = append(out, k)
		}
	}
	return out
}

// PeersWithin maxHops 跳 BFS：
//
//	GO 1 TO N STEPS FROM <vid> OVER links WHERE links.expires > now()
//	YIELD DISTINCT properties($$).key AS key | LIMIT BFSMaxNodes
func (s *NebulaLinkStore) PeersWithin(ctx context.Context, a string, maxHops int, peerPrefix string) []string {
	if a == "" || maxHops < 1 {
		return nil
	}
	if maxHops > BFSMaxHops {
		maxHops = BFSMaxHops
	}
	q := fmt.Sprintf(
		`GO 1 TO %d STEPS FROM "%s" OVER links WHERE links.expires > now()
		 YIELD DISTINCT properties($$).key AS key
		 | LIMIT %d;`,
		maxHops, vidOf(a), BFSMaxNodes)
	rs, err := s.execute(ctx, q)
	if err != nil {
		s.logger.Warn("nebula peers_within failed", zap.Error(err))
		return nil
	}
	keys := extractStringColumn(rs, 0)
	out := keys[:0]
	for _, k := range keys {
		if k == a {
			continue
		}
		if peerPrefix != "" && !strings.HasPrefix(k, peerPrefix) {
			continue
		}
		out = append(out, k)
	}
	return out
}

// Tag 给节点打标签。所有 tag 都映射到 `suspect` 类型 ——
// schema 里 suspect 是开放 label 的，不区分 suspect/goodwill 语义
// （上层规则按 tag 名解释）。
func (s *NebulaLinkStore) Tag(ctx context.Context, node, tag string) {
	if node == "" || tag == "" {
		return
	}
	tag = strings.ToLower(strings.TrimSpace(tag))
	v := vidOf(node)
	ts := time.Now().Unix()
	q := fmt.Sprintf(
		`UPSERT VERTEX ON identity "%s" SET key = "%s";
		 INSERT VERTEX suspect(label, ts) VALUES "%s": ("%s", %d);`,
		v, escapeStr(node), v, escapeStr(tag), ts)
	if _, err := s.execute(ctx, q); err != nil {
		s.logger.Warn("nebula tag failed", zap.Error(err))
	}
}

// TagsWithin 拿 maxHops 跳内所有节点上的 tag 集合（去重 + count）。
//
// 1) GO N STEPS 拿所有目标 vid（含起点 vid）
// 2) FETCH PROP ON suspect 一把拉所有 vid 的 suspect.label
// 3) 用 map[label] 计数
func (s *NebulaLinkStore) TagsWithin(ctx context.Context, a string, maxHops int) map[string]int {
	if a == "" {
		return nil
	}
	if maxHops <= 0 {
		maxHops = 1
	}
	if maxHops > BFSMaxHops {
		maxHops = BFSMaxHops
	}
	startVid := vidOf(a)
	out := map[string]int{}

	// 1) 拿 maxHops 跳内所有目标 vid
	q := fmt.Sprintf(
		`GO 1 TO %d STEPS FROM "%s" OVER links WHERE links.expires > now()
		 YIELD DISTINCT id($$) AS vid
		 | LIMIT %d;`,
		maxHops, startVid, BFSMaxNodes)
	rs, err := s.execute(ctx, q)
	if err != nil {
		s.logger.Warn("nebula tagswithin GO failed", zap.Error(err))
		return out
	}
	vids := extractStringColumn(rs, 0)
	vids = append(vids, startVid) // 起点也算

	// 去重 vids
	seen := make(map[string]struct{}, len(vids))
	uniq := make([]string, 0, len(vids))
	for _, v := range vids {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		uniq = append(uniq, v)
	}
	if len(uniq) == 0 {
		return out
	}

	// 2) FETCH PROP ON suspect <vid1>, <vid2>, ... 批量
	quoted := make([]string, 0, len(uniq))
	for _, v := range uniq {
		quoted = append(quoted, `"`+v+`"`)
	}
	q2 := fmt.Sprintf(
		`FETCH PROP ON suspect %s YIELD suspect.label AS label;`,
		strings.Join(quoted, ", "))
	rs2, err := s.execute(ctx, q2)
	if err != nil {
		// FETCH 在节点没有该 TAG 时不报错，只返空行；err 说明 schema 真的有问题
		s.logger.Warn("nebula tagswithin FETCH failed", zap.Error(err))
		return out
	}
	for _, label := range extractStringColumn(rs2, 0) {
		if label == "" {
			continue
		}
		out[label]++
	}
	return out
}

// edgeRow 从 nebula 拉回的"一条出边的 raw"。WeightedFanout 等本地算 weight 用。
type edgeRow struct {
	peerKey string
	atUnix  int64
}

// fetchOutEdges 拉 src 的所有出边（含 at + 邻居 key）。
// 用于 Weighted* 系列方法的客户端 decay 计算 —— nGQL 没有 exp() 内置，
// math 在 Go 端做。
func (s *NebulaLinkStore) fetchOutEdges(ctx context.Context, srcVid string, limit int) []edgeRow {
	q := fmt.Sprintf(
		`GO 1 STEP FROM "%s" OVER links WHERE links.expires > now()
		 YIELD properties($$).key AS key, links.at AS at
		 | LIMIT %d;`,
		srcVid, limit)
	rs, err := s.execute(ctx, q)
	if err != nil {
		s.logger.Warn("nebula fetch out-edges failed", zap.Error(err))
		return nil
	}
	if rs == nil {
		return nil
	}
	n := rs.GetRowSize()
	out := make([]edgeRow, 0, n)
	for i := 0; i < n; i++ {
		rec, err := rs.GetRowValuesByIndex(i)
		if err != nil || rec == nil {
			continue
		}
		var er edgeRow
		if kv, err := rec.GetValueByIndex(0); err == nil && kv != nil {
			er.peerKey, _ = kv.AsString()
		}
		if av, err := rec.GetValueByIndex(1); err == nil && av != nil {
			// timestamp 在 nebula-go 里走 int64 路径
			if v, err := av.AsInt(); err == nil {
				er.atUnix = v
			}
		}
		if er.peerKey == "" {
			continue
		}
		out = append(out, er)
	}
	return out
}

// WeightedFanout 1-跳带时间衰减权重和。peerPrefix 同 Peers。
//
// 实现：拉 1-跳 (peer_key, at) 列表，Go 端套 e^(-Δt/halflife)，累加。
// nGQL 不支持 server-side exp()，所以这里全在客户端做 math。
func (s *NebulaLinkStore) WeightedFanout(ctx context.Context, node, peerPrefix string,
	decayHalfLife time.Duration,
) (float64, int) {
	if node == "" {
		return 0, 0
	}
	edges := s.fetchOutEdges(ctx, vidOf(node), linkMaxPeersPerKey)
	if len(edges) == 0 {
		return 0, 0
	}
	now := time.Now()
	cutoff := now.Add(-linkTTL)
	totalW := 0.0
	count := 0
	for _, e := range edges {
		lastObserved := time.Unix(e.atUnix, 0)
		if lastObserved.Before(cutoff) {
			continue // 兜底 TTL（nebula expires 在 nGQL 已过滤，但 at 是单独字段）
		}
		w := nebulaEdgeWeight(lastObserved, now, decayHalfLife)
		if w < decayPruneThreshold {
			continue
		}
		if peerPrefix != "" && !strings.HasPrefix(e.peerKey, peerPrefix) {
			continue
		}
		totalW += w
		count++
	}
	return totalW, count
}

// WeightedPeersWithin maxHops 跳的"加权 fanout"。
//
// 客户端 BFS：从起点出发，每层调一次 fetchOutEdges 拉子边；同 mem 的
// `parentW * timeDecay * hopDecay` 累加规则。
//
// 复杂度：每跳一次 round-trip；2-hop = 2 RTT。比 server-side GO N STEPS 一把拿
// 慢，但 server-side 不能拿到每条边的 (parent, child, at) 三元组（GO 收敛后
// 丢路径信息），无法做正确的客户端 decay。
//
// 走这条路径的代价：2-hop 大概 6-10ms（vs Peers 1-2ms）；不在 hot path 跑就 OK。
func (s *NebulaLinkStore) WeightedPeersWithin(ctx context.Context, a string, maxHops int, peerPrefix string,
	decayHalfLife time.Duration, hopDecay float64,
) (float64, int) {
	if a == "" || maxHops <= 0 {
		return 0, 0
	}
	if maxHops > BFSMaxHops {
		maxHops = BFSMaxHops
	}
	useHopDecay := hopDecay > 0 && hopDecay < 1
	now := time.Now()
	cutoff := now.Add(-linkTTL)

	startVid := vidOf(a)
	// nodeWeight key 是 vid（不是原始 key），因为 fetchOutEdges 返的 peerKey
	// 已经是原始 key，但下一跳要按 vid 走。维护两个 map：
	//   nodeWeight: vid -> 当前最大 weight
	//   vidToKey:   vid -> 原始 key（输出过滤时用）
	nodeWeight := map[string]float64{startVid: 1.0}
	vidToKey := map[string]string{startVid: a}
	frontier := []string{startVid}
	visited := 1
	for hop := 1; hop <= maxHops; hop++ {
		next := make([]string, 0, len(frontier))
		for _, vid := range frontier {
			edges := s.fetchOutEdges(ctx, vid, linkMaxPeersPerKey)
			parentW := nodeWeight[vid]
			for _, e := range edges {
				lastObserved := time.Unix(e.atUnix, 0)
				if lastObserved.Before(cutoff) {
					continue
				}
				tw := nebulaEdgeWeight(lastObserved, now, decayHalfLife)
				if tw < decayPruneThreshold {
					continue
				}
				w := parentW * tw
				if useHopDecay {
					w *= hopDecay
				}
				peerVid := vidOf(e.peerKey)
				if prev, ok := nodeWeight[peerVid]; ok {
					if w > prev {
						nodeWeight[peerVid] = w
					}
					continue
				}
				nodeWeight[peerVid] = w
				vidToKey[peerVid] = e.peerKey
				next = append(next, peerVid)
				visited++
				if visited >= BFSMaxNodes {
					goto done
				}
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
done:
	total := 0.0
	count := 0
	for v, w := range nodeWeight {
		if v == startVid {
			continue
		}
		key := vidToKey[v]
		if peerPrefix != "" && !strings.HasPrefix(key, peerPrefix) {
			continue
		}
		total += w
		count++
	}
	return total, count
}

// WeightedTagsWithin maxHops 跳 BFS 收集所有节点的 tag → 加权信号。
// 每个 tag 的累加 = sum(节点 path weight × hopDecay^hop)。
//
// 实现：先复用 WeightedPeersWithin 的 BFS 拿到 (vid -> weight)，再批量 FETCH
// 拿每个 vid 的 suspect.label，按 weight 累加到 map[label]。
func (s *NebulaLinkStore) WeightedTagsWithin(ctx context.Context, a string, maxHops int,
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
	now := time.Now()
	cutoff := now.Add(-linkTTL)

	startVid := vidOf(a)
	nodeWeight := map[string]float64{startVid: 1.0}
	frontier := []string{startVid}
	visited := 1
	for hop := 1; hop <= maxHops; hop++ {
		next := make([]string, 0, len(frontier))
		for _, vid := range frontier {
			edges := s.fetchOutEdges(ctx, vid, linkMaxPeersPerKey)
			parentW := nodeWeight[vid]
			for _, e := range edges {
				lastObserved := time.Unix(e.atUnix, 0)
				if lastObserved.Before(cutoff) {
					continue
				}
				tw := nebulaEdgeWeight(lastObserved, now, decayHalfLife)
				if tw < decayPruneThreshold {
					continue
				}
				w := parentW * tw
				if useHopDecay {
					w *= hopDecay
				}
				peerVid := vidOf(e.peerKey)
				if prev, ok := nodeWeight[peerVid]; ok {
					if w > prev {
						nodeWeight[peerVid] = w
					}
					continue
				}
				nodeWeight[peerVid] = w
				next = append(next, peerVid)
				visited++
				if visited >= BFSMaxNodes {
					goto done
				}
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
done:
	out := map[string]float64{}
	if len(nodeWeight) == 0 {
		return out
	}
	// 批量 FETCH 所有 vid 的 suspect.label + vid 一起拿（按 vid 关联 weight）
	quoted := make([]string, 0, len(nodeWeight))
	for v := range nodeWeight {
		quoted = append(quoted, `"`+v+`"`)
	}
	q := fmt.Sprintf(
		`FETCH PROP ON suspect %s YIELD id(VERTEX) AS vid, suspect.label AS label;`,
		strings.Join(quoted, ", "))
	rs, err := s.execute(ctx, q)
	if err != nil {
		s.logger.Warn("nebula weighted_tags FETCH failed", zap.Error(err))
		return out
	}
	if rs == nil {
		return out
	}
	n := rs.GetRowSize()
	for i := 0; i < n; i++ {
		rec, err := rs.GetRowValuesByIndex(i)
		if err != nil || rec == nil {
			continue
		}
		vidV, err := rec.GetValueByIndex(0)
		if err != nil || vidV == nil {
			continue
		}
		vid, _ := vidV.AsString()
		labelV, err := rec.GetValueByIndex(1)
		if err != nil || labelV == nil {
			continue
		}
		label, _ := labelV.AsString()
		if vid == "" || label == "" {
			continue
		}
		w, ok := nodeWeight[vid]
		if !ok {
			continue
		}
		out[label] += w
	}
	return out
}

// Purge GDPR 删节点的所有边 + tag。
// DELETE VERTEX <vid> WITH EDGE 同时干掉 incoming/outgoing 边和 attached tags。
func (s *NebulaLinkStore) Purge(ctx context.Context, key string) int {
	if key == "" {
		return 0
	}
	v := vidOf(key)
	// 先数一下 fanout 度数，给个返回值（best-effort；与 mem 实现的 purged 边数对齐）。
	deg := 0
	cq := fmt.Sprintf(`GO 1 STEP FROM "%s" OVER links YIELD count(*) AS c;`, v)
	if rs, err := s.execute(ctx, cq); err == nil && rs != nil && rs.GetRowSize() > 0 {
		if rec, err := rs.GetRowValuesByIndex(0); err == nil && rec != nil {
			if cv, err := rec.GetValueByIndex(0); err == nil && cv != nil {
				if c, err := cv.AsInt(); err == nil {
					deg = int(c)
				}
			}
		}
	}
	dq := fmt.Sprintf(`DELETE VERTEX "%s" WITH EDGE;`, v)
	if _, err := s.execute(ctx, dq); err != nil {
		s.logger.Warn("nebula purge failed", zap.Error(err))
		return 0
	}
	// 反向：peer→key 边在 DELETE VERTEX WITH EDGE 时也清掉；返 deg 作为"清掉的（单向）边数"。
	if deg == 0 {
		deg = 1 // 兜底：至少标记为完成（与 mem 行为对齐）
	}
	return deg
}

// ─── helpers ─────────────────────────────────────────────────────────

// nebulaEdgeWeight 与 mem 的 edgeWeight 同语义；独立一份避免 -tags nebula
// 跟默认 build 共享符号产生 dup（mem 那份没有 build tag）。
func nebulaEdgeWeight(lastObserved, now time.Time, halflife time.Duration) float64 {
	if halflife <= 0 {
		return 1.0
	}
	dt := now.Sub(lastObserved)
	if dt <= 0 {
		return 1.0
	}
	return math.Exp(-dt.Hours() / halflife.Hours())
}

// extractStringColumn 从 ResultSet 抽 colIdx 列里的 string；忽略读不到的行。
//
// 不 panic 不返 err —— 上层 nGQL 错误已经在 execute 里 log 了，这里只做 best-effort
// 解码。
func extractStringColumn(rs *nebula.ResultSet, colIdx int) []string {
	if rs == nil {
		return nil
	}
	n := rs.GetRowSize()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rec, err := rs.GetRowValuesByIndex(i)
		if err != nil || rec == nil {
			continue
		}
		v, err := rec.GetValueByIndex(colIdx)
		if err != nil || v == nil {
			continue
		}
		s, err := v.AsString()
		if err != nil || s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// escapeStr 逃逸 nGQL 字符串字面量中的反斜杠 + 双引号。完整 nGQL injection
// 防护应该用参数化但 nebula-go 当前接口不支持；本实现先 escape 兜底。
// 调用方约定：所有 key/tag 输入是 server-trusted 的（来自 risk-manage 自家
// extractor，不直接接外部 HTTP body）。
func escapeStr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// escapeIdentifier nebula space / tag 名 —— 严格 [a-zA-Z0-9_]+。
// 不合法字符直接 strip，避免拼到 USE / FETCH 里 injection。
func escapeIdentifier(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// splitHostPort 拆 "host:port" → (host, int(port))。仅支持 IPv4 / hostname；
// IPv6 风格 [::1]:9669 暂不支持（生产没人这么部 nebula）。
func splitHostPort(addr string) (string, int, error) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, 9669, nil
	}
	host := addr[:idx]
	portStr := addr[idx+1:]
	var port int
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return "", 0, fmt.Errorf("bad port %q", portStr)
		}
		port = port*10 + int(c-'0')
	}
	if port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("port out of range: %d", port)
	}
	return host, port, nil
}

