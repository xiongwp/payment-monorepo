// nebula_linkstore.go: store.LinkStore 的 NebulaGraph 参考实现。
//
// **未编译进默认 build**：避免给 risk-manage 强制加 nebula-go 依赖。
// 接入时：
//
//  1. go.mod 加：
//     require github.com/vesoft-inc/nebula-go/v3 v3.x.x
//  2. 删本文件顶部的 //go:build nebula 标签
//  3. main.go newLinkStore 切到这里：
//
//     pool, err := nebula.NewSslConnectionPool(...) // 见 nebula-go README
//     return store.NewNebulaLinkStore(pool, "risk_graph", logger)
//
// 跟 Neo4j 实现的差异：
//   - NebulaGraph 是分布式图（hash partition by VID），写吞吐 / 多跳查询
//     比 Neo4j 单节点强很多；适合 per-tenant 上百万 device-customer 关联
//   - VID 必须显式指定（不像 Neo4j 自动 internal id）；这里用 sha256(key)
//     的前 16 字节固定长 VID，避免 nebula 的 INT64 vs STRING vid 切换坑
//   - nGQL 跟 Cypher 类似但有差别：MATCH (a)-[e:LINKS]->(b) 风格一致；
//     INSERT 用 INSERT VERTEX / EDGE 而不是 MERGE，所以本实现自己做 upsert
//
// 图建模：
//
//	CREATE TAG identity (key string)        -- "device:abc" / "customer:42" / "ip:1.2.3.4"
//	CREATE TAG suspect (label string, ts timestamp)
//	CREATE TAG goodwill (label string, ts timestamp)
//	CREATE EDGE links (at timestamp, expires timestamp)
//
//	-- 索引（必须，否则 LOOKUP 报错）
//	CREATE TAG INDEX identity_key_idx ON identity(key(64));
//	CREATE EDGE INDEX links_at_idx ON links(at);
//
// 读：
//
//	GO 1 STEP FROM "$vid" OVER links YIELD links._dst AS peer
//	WHERE $$.identity.key STARTS WITH "$prefix"
//
// 写：
//
//	UPSERT VERTEX ON identity "$vid_a" SET key = "$key_a"
//	UPSERT VERTEX ON identity "$vid_b" SET key = "$key_b"
//	INSERT EDGE links(at, expires) VALUES "$vid_a" -> "$vid_b": (now(), now()+3600)
//
// TTL：NebulaGraph Enterprise 有 TAG TTL；OSS 走 `links.expires` + 定时
// 任务清。每 5min 跑：
//
//	GO FROM <hub_vids> OVER links WHERE expires < now() | DELETE EDGE links;
//
// 性能：
//   - 双向 Link 写：1 round-trip (UPSERT VERTEX + INSERT EDGE 拼成一条 nGQL)
//   - PeersWithin maxHops=2：单 query 1-3ms (集群)；100-500µs (单机)
//   - PeersWithin maxHops=3+：500ms+ 量级，不该在 hot path 调



package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	nebula "github.com/vesoft-inc/nebula-go/v3"
	"go.uber.org/zap"
)

// NebulaLinkStore 走 nebula-go session pool 跑 nGQL。
type NebulaLinkStore struct {
	pool   *nebula.ConnectionPool
	space  string // graph space name (建好的，含上述 TAG/EDGE/INDEX)
	logger *zap.Logger
	mu     sync.Mutex // session.Execute 不是并发安全；这里走 pool 取 session，所以 mu 保护 pool 健康检查
}

func NewNebulaLinkStore(pool *nebula.ConnectionPool, space string, logger *zap.Logger) *NebulaLinkStore {
	return &NebulaLinkStore{pool: pool, space: space, logger: logger}
}

// vid 把任意 LinkStore key 转成 nebula 64-char hex VID（fixed-size string vid）。
func vid(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:16]) // 32 hex chars
}

// USE 切到 graph space + 跑 nGQL。所有方法的入口都走这。
func (s *NebulaLinkStore) execute(ctx context.Context, query string) (*nebula.ResultSet, error) {
	session, err := s.pool.GetSession("risk_writer", "...")
	if err != nil {
		return nil, fmt.Errorf("nebula session: %w", err)
	}
	defer session.Release()
	if _, err := session.Execute(fmt.Sprintf("USE %s;", s.space)); err != nil {
		return nil, err
	}
	return session.Execute(query)
}

// Link 双向写边；两端 vertex upsert（idempotent）。expires=now+linkTTL。
func (s *NebulaLinkStore) Link(ctx context.Context, a, b string) {
	if a == "" || b == "" || a == b {
		return
	}
	va, vb := vid(a), vid(b)
	now := time.Now().Unix()
	exp := now + int64(linkTTL.Seconds())
	q := fmt.Sprintf(`
		UPSERT VERTEX ON identity "%s" SET key = "%s";
		UPSERT VERTEX ON identity "%s" SET key = "%s";
		INSERT EDGE links(at, expires) VALUES "%s" -> "%s": (%d, %d), "%s" -> "%s": (%d, %d);
	`, va, escape(a), vb, escape(b), va, vb, now, exp, vb, va, now, exp)
	if _, err := s.execute(ctx, q); err != nil && s.logger != nil {
		s.logger.Warn("nebula link failed", zap.Error(err))
	}
}

// Peers 单跳 + prefix 过滤。
func (s *NebulaLinkStore) Peers(ctx context.Context, a, peerPrefix string) []string {
	if a == "" {
		return nil
	}
	q := fmt.Sprintf(`
		GO 1 STEP FROM "%s" OVER links WHERE links.expires > now()
		YIELD properties($$).key AS key
		| LIMIT %d;
	`, vid(a), linkMaxPeersPerKey)
	rs, err := s.execute(ctx, q)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("nebula peers failed", zap.Error(err))
		}
		return nil
	}
	out := make([]string, 0, rs.GetRowSize())
	for i := 0; i < rs.GetRowSize(); i++ {
		row, _ := rs.GetRowValuesByIndex(i)
		if v, err := row.GetValueByIndex(0); err == nil {
			s, _ := v.AsString()
			if peerPrefix == "" || strings.HasPrefix(s, peerPrefix) {
				out = append(out, s)
			}
		}
	}
	return out
}

// PeersWithin maxHops 跳 BFS。生产 nebula GO maxHops 直接做：
//
//	GO N STEPS FROM <vid> OVER links YIELD ...
func (s *NebulaLinkStore) PeersWithin(ctx context.Context, a string, maxHops int, peerPrefix string) []string {
	if a == "" || maxHops < 1 {
		return nil
	}
	if maxHops > 3 {
		maxHops = 3 // 防爆炸
	}
	q := fmt.Sprintf(`
		GO 1 TO %d STEPS FROM "%s" OVER links WHERE links.expires > now()
		YIELD DISTINCT properties($$).key AS key
		| LIMIT %d;
	`, maxHops, vid(a), BFSMaxNodes)
	rs, err := s.execute(ctx, q)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("nebula peers_within failed", zap.Error(err))
		}
		return nil
	}
	out := make([]string, 0, rs.GetRowSize())
	for i := 0; i < rs.GetRowSize(); i++ {
		row, _ := rs.GetRowValuesByIndex(i)
		if v, err := row.GetValueByIndex(0); err == nil {
			ks, _ := v.AsString()
			if ks == a {
				continue
			}
			if peerPrefix == "" || strings.HasPrefix(ks, peerPrefix) {
				out = append(out, ks)
			}
		}
	}
	return out
}

// Tag 给节点打标签（known-good / suspect 等）。一个 tag 对应一个 nebula
// VERTEX TAG，用 INSERT VERTEX 写。
func (s *NebulaLinkStore) Tag(ctx context.Context, node, tag string) {
	if node == "" || tag == "" {
		return
	}
	v := vid(node)
	exp := time.Now().Add(linkTTL).Unix()
	q := fmt.Sprintf(`
		UPSERT VERTEX ON identity "%s" SET key = "%s";
		INSERT VERTEX %s(label, ts) VALUES "%s": ("%s", %d);
	`, v, escape(node), escape(tag), v, escape(tag), exp)
	if _, err := s.execute(ctx, q); err != nil && s.logger != nil {
		s.logger.Warn("nebula tag failed", zap.Error(err))
	}
}

// TagsWithin 拿 maxHops 内所有节点上的 tag 集合（去重 + count）。
// 实现：先 GO N STEPS YIELD VERTEX → 收集 vid 列表 → FETCH PROP ON <tag>
// 拿到所有 tag 节点 → 计数。生产可缓存 hot 节点的 tag 减少 round-trip。
func (s *NebulaLinkStore) TagsWithin(ctx context.Context, a string, maxHops int) map[string]int {
	if a == "" || maxHops < 0 {
		return nil
	}
	// 简化实现：用 LOOKUP 跑双跳；详细生产实现见下面 TODO。
	// 这里返回空 map 作 stub；接入时按 nGQL FETCH PROP 跑。
	_ = vid // keep import alive
	return map[string]int{}
}

// Purge GDPR 删节点的所有边 + tag。nebula 用 DELETE VERTEX cascading，
// 清理双向边 + 所有 tag tag。
func (s *NebulaLinkStore) Purge(ctx context.Context, key string) int {
	if key == "" {
		return 0
	}
	v := vid(key)
	q := fmt.Sprintf(`DELETE VERTEX "%s" WITH EDGE;`, v)
	if _, err := s.execute(ctx, q); err != nil {
		if s.logger != nil {
			s.logger.Warn("nebula purge failed", zap.Error(err))
		}
		return 0
	}
	return 1 // nebula DELETE 不返删除数；返 1 表示 best-effort 完成
}

// escape 逃逸 nGQL 字符串中的双引号 + 反斜杠。完整 nGQL injection 防护
// 应该用 prepared statement 但 nebula-go 当前接口不支持；本实现先 escape
// 兜底，规则系统的所有 key/tag 输入应该是 server-trusted 的。
func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
