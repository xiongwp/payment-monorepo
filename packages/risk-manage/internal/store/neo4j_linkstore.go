// neo4j_linkstore.go: store.LinkStore 的 Neo4j 参考实现。
//
// **未编译进默认 build**：避免给 risk-manage 强制加 neo4j-driver 依赖。
// 接入时：
//
//  1. go.mod 加：
//     require github.com/neo4j/neo4j-go-driver/v5 v5.x.x
//  2. 删除文件顶部的 //go:build neo4j 标签
//  3. main.go newLinkStore 切到这里：
//
//     drv, _ := neo4j.NewDriverWithContext("neo4j://neo4j:7687",
//         neo4j.BasicAuth("neo4j", os.Getenv("NEO4J_PWD"), ""))
//     return store.NewNeo4jLinkStore(drv)
//
// 图建模：
//
//	(:Identity {key:'device:abc'}) -[:LINKS {at: timestamp(), expires: ts}]-> (:Identity {key:'customer:42'})
//
// 写入是 MERGE 双向边（idempotent）；读用 MATCH 拿邻居 by prefix。生产记得：
//   - Identity.key 加唯一约束 + 索引
//   - LINKS.expires 加 TTL 任务（Neo4j Aura 有 native TTL；OSS 用定时任务清）
//   - 关联爆炸保护：MATCH ... LIMIT 1024（同 MemLinkStore）
//
// 适用场景：
//   - 商业版到了"几千万 device + 上亿条 link"规模时，MemLinkStore（map）扛不住，
//     Neo4j 的 path / community detection 才是 fraud ring 真正能做出来的东西
//   - 小规模部署（< 1M edges）继续用 MemLinkStore 即可

//go:build neo4j

package store

import (
	"context"
	"sort"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

const linkTTLNeo4j = time.Hour

type Neo4jLinkStore struct {
	drv neo4j.DriverWithContext
}

func NewNeo4jLinkStore(drv neo4j.DriverWithContext) *Neo4jLinkStore {
	return &Neo4jLinkStore{drv: drv}
}

func (s *Neo4jLinkStore) Link(ctx context.Context, a, b string) {
	if a == "" || b == "" || a == b {
		return
	}
	expires := time.Now().Add(linkTTLNeo4j).Unix()
	sess := s.drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	_, _ = sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		// MERGE 双向边；过期戳每次写入更新（"持续观察到关联"延长 TTL）
		_, err := tx.Run(ctx, `
            MERGE (x:Identity {key: $a})
            MERGE (y:Identity {key: $b})
            MERGE (x)-[r1:LINKS]->(y) SET r1.expires = $exp
            MERGE (y)-[r2:LINKS]->(x) SET r2.expires = $exp
        `, map[string]any{"a": a, "b": b, "exp": expires})
		return nil, err
	})
}

func (s *Neo4jLinkStore) Peers(ctx context.Context, a, prefix string) []string {
	if a == "" {
		return nil
	}
	now := time.Now().Unix()
	sess := s.drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		// 过滤掉过期边；prefix 在客户端做（Cypher prefix 也可走 STARTS WITH，
		// 但需 schema 索引；当前简化）
		out, err := tx.Run(ctx, `
            MATCH (x:Identity {key: $a})-[r:LINKS]->(y:Identity)
            WHERE r.expires >= $now
            RETURN y.key AS key
            LIMIT 1024
        `, map[string]any{"a": a, "now": now})
		if err != nil {
			return nil, err
		}
		var keys []string
		for out.Next(ctx) {
			rec := out.Record()
			if v, ok := rec.Get("key"); ok {
				if s, ok := v.(string); ok {
					if prefix == "" || (len(s) >= len(prefix) && s[:len(prefix)] == prefix) {
						keys = append(keys, s)
					}
				}
			}
		}
		sort.Strings(keys)
		return keys, nil
	})
	if err != nil {
		return nil
	}
	return res.([]string)
}
