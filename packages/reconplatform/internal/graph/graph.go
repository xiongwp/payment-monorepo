// Package graph 跨服务事件关联图谱。
//
// 业务问题：用户报"我这笔 PI 出问题了"，运营只有 pi_id；但一笔支付涉及
//   - order-core.payment_intents
//   - order-core.charges
//   - payment-channel.acquirer_tx
//   - accounting-system.account_transaction
//   - accounting-system.voucher
//   - merchant-side webhook_outbox
// ... 至少 5+ 个服务的 N 张表，需要凭脑子拼链路。
//
// Graph 用 reconplatform Redis 已有的索引（recon:idx:<col>:<value>）做 BFS：
// 从 (idx_name, value) 起始，拉所有引用该 value 的 event；对每个 event 再
// 看它自己的 indexes（trace_id / merchant_id / pi_id 等），加进队列继续扩张，
// 直到 depth 上限或事件总数上限。
//
// 输出 JSON：
//
//	{
//	  "nodes": [
//	    {"id": "order-core:payment_intents:pi_xxx", "svc": "order-core",
//	     "table": "payment_intents", "pk": "pi_xxx", "data": {...}, "ts": "..."},
//	    ...
//	  ],
//	  "edges": [
//	    {"src": "<node_id_a>", "dst": "<node_id_b>", "via": "pi_id"},
//	    ...
//	  ],
//	  "stats": {"depth_reached": 3, "nodes": 8, "truncated": false}
//	}
//
// admin web 用 cytoscape.js / dagre 画成左到右的 DAG，节点颜色按 svc 区分。
package graph

import (
	"context"
	"fmt"
	"sort"

	"reconcile-system/internal/store"
)

// Node 一个事件。
type Node struct {
	ID        string         `json:"id"`        // <svc>:<table>:<pk>
	Service   string         `json:"svc"`
	Table     string         `json:"table"`
	PK        string         `json:"pk"`
	Op        string         `json:"op"`
	Timestamp string         `json:"ts"`        // RFC3339
	Data      map[string]any `json:"data"`      // After 行内容
	Indexes   map[string]string `json:"indexes"` // 该事件携带的索引（trace_id 等）
}

// Edge 节点间通过共享某 idx 关联。
type Edge struct {
	Src string `json:"src"` // node id
	Dst string `json:"dst"`
	Via string `json:"via"` // 哪个 idx_col 把它们连起来 (pi_id / trace_id / merchant_id)
}

// Stats BFS 扩展统计。
type Stats struct {
	DepthReached int  `json:"depth_reached"`
	Nodes        int  `json:"nodes"`
	Edges        int  `json:"edges"`
	Truncated    bool `json:"truncated"` // 是否因为 cap 提前停
}

// Result Build 返回值。
type Result struct {
	Nodes []*Node `json:"nodes"`
	Edges []*Edge `json:"edges"`
	Stats Stats   `json:"stats"`
}

// Builder 状态化遍历器（一次 Build 用一次，线程不安全）。
type Builder struct {
	searcher *store.Searcher
	maxDepth int
	maxNodes int
}

// New 构造。maxDepth 默认 3（PI → charge → ledger 一般 3 跳够）；
// maxNodes 默认 200（避免 trace_id 关联太多事件爆炸）。
func New(s *store.Searcher) *Builder {
	return &Builder{
		searcher: s,
		maxDepth: 3,
		maxNodes: 200,
	}
}

// WithLimits 自定义 maxDepth / maxNodes。
func (b *Builder) WithLimits(depth, nodes int) *Builder {
	if depth > 0 {
		b.maxDepth = depth
	}
	if nodes > 0 {
		b.maxNodes = nodes
	}
	return b
}

// Build 从 (idx_name, value) 开始 BFS 拉关联事件。
//
// 算法：
//   1. layer 0：SearchByIndex(idx_name, value) 拿初始事件集
//   2. layer N (1..maxDepth)：对上一层每个事件，看它的 Indexes map，
//      对每个 (next_idx, next_val)，再 SearchByIndex 一次，新事件入下一层
//   3. 节点去重靠 ID = svc:table:pk
//   4. 边去重靠 (src, dst, via)
//
// 截断条件（任一满足）：depth 到上限 / nodes 到 maxNodes。
func (b *Builder) Build(ctx context.Context, idxName, value string) (*Result, error) {
	r := &Result{
		Nodes: []*Node{},
		Edges: []*Edge{},
	}
	visited := make(map[string]*Node) // id → node
	edgeKey := func(src, dst, via string) string {
		if src > dst {
			src, dst = dst, src // 无向去重
		}
		return src + "|" + dst + "|" + via
	}
	edges := make(map[string]bool)

	// 入队 layer 0 — 直接搜索 (idxName, value)
	cur, err := b.searcher.SearchByIndex(ctx, idxName, value)
	if err != nil {
		return nil, fmt.Errorf("layer 0 search: %w", err)
	}
	frontier := make([]*Node, 0, len(cur))
	for _, e := range cur {
		n := eventToNode(e)
		if _, exist := visited[n.ID]; exist {
			continue
		}
		visited[n.ID] = n
		frontier = append(frontier, n)
	}

	for depth := 1; depth <= b.maxDepth; depth++ {
		if len(frontier) == 0 {
			r.Stats.DepthReached = depth - 1
			break
		}
		if len(visited) >= b.maxNodes {
			r.Stats.Truncated = true
			r.Stats.DepthReached = depth - 1
			break
		}

		next := make([]*Node, 0, len(frontier))
		for _, n := range frontier {
			// 对当前节点的每个 idx 再搜一次
			for idxCol, idxVal := range n.Indexes {
				if idxVal == "" {
					continue
				}
				peers, err := b.searcher.SearchByIndex(ctx, idxCol, idxVal)
				if err != nil {
					continue // 单 idx 失败不阻塞整图
				}
				for _, e := range peers {
					p := eventToNode(e)
					existing, exist := visited[p.ID]
					// 不论新旧都加边（同 idx 把当前节点和其他节点连起来）
					if existing != nil && existing.ID != n.ID {
						k := edgeKey(n.ID, existing.ID, idxCol)
						if !edges[k] {
							edges[k] = true
							r.Edges = append(r.Edges, &Edge{Src: n.ID, Dst: existing.ID, Via: idxCol})
						}
					} else if !exist && p.ID != n.ID {
						visited[p.ID] = p
						next = append(next, p)
						r.Edges = append(r.Edges, &Edge{Src: n.ID, Dst: p.ID, Via: idxCol})
						edges[edgeKey(n.ID, p.ID, idxCol)] = true
						if len(visited) >= b.maxNodes {
							r.Stats.Truncated = true
							goto done
						}
					}
				}
			}
		}
		r.Stats.DepthReached = depth
		frontier = next
	}

done:
	// 输出 nodes（按 ts 升序，便于 admin web 展示时间线）
	for _, n := range visited {
		r.Nodes = append(r.Nodes, n)
	}
	sort.Slice(r.Nodes, func(i, j int) bool {
		return r.Nodes[i].Timestamp < r.Nodes[j].Timestamp
	})
	r.Stats.Nodes = len(r.Nodes)
	r.Stats.Edges = len(r.Edges)
	return r, nil
}

// eventToNode 把 store.Event 包装成 graph.Node。
func eventToNode(e *store.Event) *Node {
	if e == nil {
		return nil
	}
	id := e.Service + ":" + e.Table + ":" + e.PK
	return &Node{
		ID:        id,
		Service:   e.Service,
		Table:     e.Table,
		PK:        e.PK,
		Op:        e.Op,
		Timestamp: e.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
		Data:      e.Row(),
		Indexes:   e.Indexes,
	}
}
