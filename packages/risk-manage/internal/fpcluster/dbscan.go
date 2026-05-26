// Package fpcluster 用 DBSCAN 对设备指纹做模糊聚类。
//
// 背景：每个新 visitor 都生成独立 ID，同一物理设备换浏览器 / 隐身模式 / 清 cookie
// 后会丢失关联。SimHash 已经能匹配同一物理设备的微小变化（汉明距离 < 8），但
// 它是 pair-wise，不能告诉"这一群指纹其实是同一台机器"。DBSCAN 在 SimHash 距离
// 空间里聚类，输出 cluster_id → 跨持久化通道的"超级 visitorID"，给规则引擎用。
//
// 算法（Ester et al., 1996）：
//   eps:    距离阈值（汉明 6 = 同设备小变化）
//   minPts: 形成 cluster 的最少点数（3 = 避免噪声）
//   核心点：邻居数 ≥ minPts 的点
//   密度可达：从核心点出发 BFS 扩展
//   噪声：既非核心点也非任何核心点的邻居
//
// 离线 vs 在线：
//   - Full DBSCAN: O(N²) 距离计算，1M 点 ~10min CPU - 走 admin endpoint
//     定时（每日凌晨）重建全量 cluster 索引。
//   - Incremental: 新 fp 到来时只跟现有 cluster centroid 比距离，相当于在线
//     最近邻分配；不重新计算 cluster 拓扑（drift 用全量定时校正）。
package fpcluster

import (
	"github.com/xiongwp/risk-manage/internal/fphash"
)

const (
	DefaultEps     = 6 // hamming distance ≤ 6 视为邻居
	DefaultMinPts  = 3 // 形成 cluster 的最少邻居数
	DefaultClusterCap = 100000 // 在线分配支持的最大 cluster 数；超出 LRU 淘汰
)

// Fingerprint DBSCAN 的输入点。
type Fingerprint struct {
	ID      string                 // device_fp / visitor_id 等业务 ID
	SimHash uint64                 // 64-bit SimHash
	Signals map[string]interface{} // 原始信号（透传，DBSCAN 不用）
}

// Cluster 一组密度可达的 Fingerprint。
type Cluster struct {
	ID         string   // 由内部分配，"cluster_<hex>" 形式
	Members    []string // Fingerprint.ID 列表
	CentroidSH uint64   // 第一个 member 的 SimHash 当 centroid（够用，DBSCAN 不要求严格中心）
}

// Result DBSCAN 全量聚类结果。
type Result struct {
	Clusters []Cluster
	Noise    []string // 未归入任何 cluster 的孤点
}

// DBSCAN 全量聚类。O(N²) — 适合定时 batch job 跑。
//
// 复杂度：N=1k → 10ms；N=10k → 1s；N=100k → 100s（CPU bound，可并行）。
// 生产 > 100k 时换 ball tree / VP tree 索引；不在本包范围。
func DBSCAN(points []Fingerprint, eps, minPts int) Result {
	if eps <= 0 {
		eps = DefaultEps
	}
	if minPts <= 0 {
		minPts = DefaultMinPts
	}
	n := len(points)
	if n == 0 {
		return Result{}
	}

	// label[i]: 0=unvisited, -1=noise, k>0=cluster id
	label := make([]int, n)
	clusterCount := 0

	for i := 0; i < n; i++ {
		if label[i] != 0 {
			continue
		}
		neighbors := regionQuery(points, i, eps)
		if len(neighbors) < minPts {
			label[i] = -1 // noise（可能后面被纳入其他 cluster，DBSCAN 允许"边界点"晋升）
			continue
		}
		clusterCount++
		label[i] = clusterCount
		expandCluster(points, label, i, neighbors, clusterCount, eps, minPts)
	}

	// 按 label 收口 Cluster
	clusters := make([]Cluster, clusterCount)
	noise := make([]string, 0)
	for i := 0; i < n; i++ {
		if label[i] == -1 {
			noise = append(noise, points[i].ID)
			continue
		}
		cid := label[i] - 1
		if len(clusters[cid].Members) == 0 {
			clusters[cid].CentroidSH = points[i].SimHash
			clusters[cid].ID = clusterIDOf(clusterCount, cid+1)
		}
		clusters[cid].Members = append(clusters[cid].Members, points[i].ID)
	}
	return Result{Clusters: clusters, Noise: noise}
}

// regionQuery 找 points[idx] eps 邻域内的所有点 index。
func regionQuery(points []Fingerprint, idx, eps int) []int {
	out := make([]int, 0, 8)
	src := points[idx].SimHash
	for j := range points {
		if j == idx {
			continue
		}
		if fphash.HammingDistance(src, points[j].SimHash) <= eps {
			out = append(out, j)
		}
	}
	return out
}

// expandCluster BFS 把所有 density-reachable 点纳入 cluster。
func expandCluster(points []Fingerprint, label []int, seed int, seedNeighbors []int, clusterID, eps, minPts int) {
	queue := append([]int{}, seedNeighbors...)
	for len(queue) > 0 {
		q := queue[0]
		queue = queue[1:]

		// 噪声点"晋升"为边界点
		if label[q] == -1 {
			label[q] = clusterID
			continue
		}
		// 已经分配过 cluster
		if label[q] != 0 {
			continue
		}
		label[q] = clusterID

		neighbors := regionQuery(points, q, eps)
		if len(neighbors) >= minPts {
			// q 本身也是核心点 → 它的邻居继续加入扩展队列
			queue = append(queue, neighbors...)
		}
	}
}

func clusterIDOf(total, idx int) string {
	// "c_<idx>" 简短可读；生产可以加 datestamp 防重启冲突
	const hex = "0123456789abcdef"
	buf := make([]byte, 0, 8)
	buf = append(buf, 'c', '_')
	for shift := 24; shift >= 0; shift -= 4 {
		v := byte(idx>>uint(shift)) & 0xf
		buf = append(buf, hex[v])
	}
	return string(buf)
}
