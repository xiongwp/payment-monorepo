// clusterstore.go — 在线 cluster 分配 + Mem/Redis 存储。
//
// 设计：把 DBSCAN 离线结果用作 "cluster centroid registry"，新指纹来时
// 走 "最近邻分配"，不重跑全量。每天凌晨 admin endpoint POST /admin/fpcluster/rebuild
// 用全量 DBSCAN 校正：merge / split / 清孤点。
//
// 跨进程：MemStore 是单机；多副本部署用 RedisStore（schema 在文件尾注释里）。
package fpcluster

import (
	"context"
	"sync"
	"time"

	"github.com/xiongwp/risk-manage/internal/fphash"
)

// ClusterStore 在线 cluster 索引。
type ClusterStore interface {
	// AssignToCluster 给一个新 fingerprint 找它属于哪个 cluster。
	// 如果距离最近 centroid ≤ eps 返回该 clusterID；否则新建 cluster。
	AssignToCluster(ctx context.Context, fp Fingerprint) (clusterID string, isNew bool, err error)

	// GetCluster 查 cluster 信息。
	GetCluster(ctx context.Context, clusterID string) (Cluster, error)

	// ReplaceAll 全量替换（rebuild job 用）。
	ReplaceAll(ctx context.Context, result Result) error

	// ClusterCount 当前 cluster 数。
	ClusterCount(ctx context.Context) (int, error)
}

// MemStore 进程内 ClusterStore 实现。
type MemStore struct {
	mu        sync.RWMutex
	clusters  map[string]*Cluster // clusterID → cluster
	memberToC map[string]string   // memberID → clusterID
	eps       int
	cap       int       // cluster 数上限；超过 LRU 淘汰最久未访问
	lastSeen  map[string]time.Time
}

func NewMemStore(eps, cap int) *MemStore {
	if eps <= 0 {
		eps = DefaultEps
	}
	if cap <= 0 {
		cap = DefaultClusterCap
	}
	return &MemStore{
		clusters:  make(map[string]*Cluster),
		memberToC: make(map[string]string),
		eps:       eps,
		cap:       cap,
		lastSeen:  make(map[string]time.Time),
	}
}

// AssignToCluster 线性扫所有 centroid 找最近的。
// 1M cluster 时 O(N) ~10ms；超过用 LSH。
func (s *MemStore) AssignToCluster(ctx context.Context, fp Fingerprint) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 已经分配过的 member 直接返回旧 cluster
	if cid, ok := s.memberToC[fp.ID]; ok {
		s.lastSeen[cid] = time.Now()
		return cid, false, nil
	}

	// 找最近 centroid
	bestID := ""
	bestDist := s.eps + 1 // 比 eps 大，找不到即新建
	for cid, cl := range s.clusters {
		d := fphash.HammingDistance(fp.SimHash, cl.CentroidSH)
		if d <= s.eps && d < bestDist {
			bestDist = d
			bestID = cid
		}
	}

	if bestID != "" {
		s.clusters[bestID].Members = append(s.clusters[bestID].Members, fp.ID)
		s.memberToC[fp.ID] = bestID
		s.lastSeen[bestID] = time.Now()
		return bestID, false, nil
	}

	// 新建 cluster
	if len(s.clusters) >= s.cap {
		s.evictOldest()
	}
	cid := clusterIDOf(0, len(s.clusters)+1) + "_" + shortTimeID()
	s.clusters[cid] = &Cluster{
		ID:         cid,
		Members:    []string{fp.ID},
		CentroidSH: fp.SimHash,
	}
	s.memberToC[fp.ID] = cid
	s.lastSeen[cid] = time.Now()
	return cid, true, nil
}

func (s *MemStore) GetCluster(ctx context.Context, clusterID string) (Cluster, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cl, ok := s.clusters[clusterID]; ok {
		return *cl, nil
	}
	return Cluster{}, ErrNotFound
}

func (s *MemStore) ReplaceAll(ctx context.Context, result Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusters = make(map[string]*Cluster, len(result.Clusters))
	s.memberToC = make(map[string]string)
	s.lastSeen = make(map[string]time.Time)
	now := time.Now()
	for i := range result.Clusters {
		cl := result.Clusters[i]
		s.clusters[cl.ID] = &cl
		s.lastSeen[cl.ID] = now
		for _, m := range cl.Members {
			s.memberToC[m] = cl.ID
		}
	}
	return nil
}

func (s *MemStore) ClusterCount(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clusters), nil
}

func (s *MemStore) evictOldest() {
	oldest := ""
	oldestT := time.Now()
	for cid, t := range s.lastSeen {
		if t.Before(oldestT) {
			oldest = cid
			oldestT = t
		}
	}
	if oldest != "" {
		cl := s.clusters[oldest]
		if cl != nil {
			for _, m := range cl.Members {
				delete(s.memberToC, m)
			}
		}
		delete(s.clusters, oldest)
		delete(s.lastSeen, oldest)
	}
}

func shortTimeID() string {
	const hex = "0123456789abcdef"
	ns := time.Now().UnixNano()
	buf := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		buf[i] = hex[ns&0xf]
		ns >>= 4
	}
	return string(buf)
}

// ErrNotFound cluster 不存在。
type ErrNotFoundType string

func (e ErrNotFoundType) Error() string { return string(e) }

var ErrNotFound ErrNotFoundType = "fpcluster: cluster not found"

// Redis schema（//go:build redis 真接入时落到 internal/store/redis_fpcluster.go）：
//   HSET risk:fpc:cluster:{id} centroid_sh <hex> members_count <n> last_seen <unix>
//   SADD risk:fpc:members:{id} {member_id}
//   HSET risk:fpc:member_to_cluster {member_id} {cluster_id}
//   ZADD risk:fpc:lru <unix> {cluster_id}     # 用 sorted set 做 LRU
//
// AssignToCluster 在线路径：
//   1. HGET risk:fpc:member_to_cluster {fp.ID} → 命中即返
//   2. miss → SCAN risk:fpc:cluster:* 取 centroids（生产用 LSH bucket 索引避免 SCAN）
//   3. 算最近邻；命中加 cluster，否则新建
