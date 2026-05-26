package fpcluster

import (
	"context"
	"testing"
)

// TestDBSCAN_TenRealDevices 模拟 100 个 fp（10 个真实设备各 10 个变种），
// eps=6 应该聚出约 10 个 cluster（允许 ±2 容差）。
func TestDBSCAN_TenRealDevices(t *testing.T) {
	points := make([]Fingerprint, 0, 100)
	// 10 个"真实设备"，每个 simhash 基础位 + 1~5 bit 噪声变种
	for dev := 0; dev < 10; dev++ {
		base := uint64(dev) * 0x1010101010101010 // 每个设备相距远
		for variant := 0; variant < 10; variant++ {
			noisedSH := base
			// 翻转 1~5 个 bit 模拟同设备小变化
			for b := 0; b < (variant%5)+1; b++ {
				noisedSH ^= 1 << uint((dev*7+variant*3+b)%64)
			}
			points = append(points, Fingerprint{
				ID:      idOf("d", dev, variant),
				SimHash: noisedSH,
			})
		}
	}
	result := DBSCAN(points, 8, 3)
	if len(result.Clusters) < 8 || len(result.Clusters) > 12 {
		t.Fatalf("expected ~10 clusters, got %d (noise=%d)", len(result.Clusters), len(result.Noise))
	}
}

func TestDBSCAN_EmptyInput(t *testing.T) {
	r := DBSCAN(nil, 6, 3)
	if len(r.Clusters) != 0 || len(r.Noise) != 0 {
		t.Fatalf("empty input should produce empty result")
	}
}

func TestDBSCAN_AllNoise(t *testing.T) {
	// 5 个互相距离很远的点 → 全 noise（minPts=3 找不到邻居）
	points := []Fingerprint{
		{ID: "a", SimHash: 0x0000000000000000},
		{ID: "b", SimHash: 0xFFFFFFFFFFFFFFFF},
		{ID: "c", SimHash: 0xAAAAAAAAAAAAAAAA},
		{ID: "d", SimHash: 0x5555555555555555},
		{ID: "e", SimHash: 0x123456789ABCDEF0},
	}
	r := DBSCAN(points, 4, 3)
	if len(r.Clusters) != 0 {
		t.Fatalf("expected 0 cluster, got %d", len(r.Clusters))
	}
	if len(r.Noise) != 5 {
		t.Fatalf("expected 5 noise, got %d", len(r.Noise))
	}
}

func TestMemStore_AssignAndReassign(t *testing.T) {
	store := NewMemStore(6, 100)
	ctx := context.Background()

	// 第一笔 → 新建
	cid1, isNew, err := store.AssignToCluster(ctx, Fingerprint{ID: "fp-a", SimHash: 0xABCD})
	if err != nil || !isNew {
		t.Fatalf("expected new cluster: isNew=%v err=%v", isNew, err)
	}

	// 同样的 fp → 复用 cluster
	cid2, isNew2, err := store.AssignToCluster(ctx, Fingerprint{ID: "fp-a", SimHash: 0xABCD})
	if err != nil || isNew2 || cid1 != cid2 {
		t.Fatalf("expected reuse: cid1=%s cid2=%s isNew=%v", cid1, cid2, isNew2)
	}

	// 近距离 fp（翻 2 bit）→ 应纳入同 cluster
	near := uint64(0xABCD) ^ 0b11 // 翻 2 bit
	cidNear, isNewNear, err := store.AssignToCluster(ctx, Fingerprint{ID: "fp-b", SimHash: near})
	if err != nil || isNewNear || cid1 != cidNear {
		t.Fatalf("expected merge into cid1: cid1=%s cidNear=%s isNew=%v", cid1, cidNear, isNewNear)
	}

	// 远距离 fp → 新 cluster
	farSH := uint64(0x1234567890ABCDEF)
	cidFar, isNewFar, err := store.AssignToCluster(ctx, Fingerprint{ID: "fp-c", SimHash: farSH})
	if err != nil || !isNewFar || cidFar == cid1 {
		t.Fatalf("expected new cluster for far fp")
	}

	n, _ := store.ClusterCount(ctx)
	if n != 2 {
		t.Fatalf("expected 2 clusters, got %d", n)
	}
}

func TestMemStore_ReplaceAll(t *testing.T) {
	store := NewMemStore(6, 100)
	ctx := context.Background()
	result := Result{
		Clusters: []Cluster{
			{ID: "c_001", Members: []string{"fp1", "fp2"}, CentroidSH: 0x1},
			{ID: "c_002", Members: []string{"fp3"}, CentroidSH: 0x2},
		},
	}
	if err := store.ReplaceAll(ctx, result); err != nil {
		t.Fatalf("replace: %v", err)
	}
	n, _ := store.ClusterCount(ctx)
	if n != 2 {
		t.Fatalf("expected 2 clusters, got %d", n)
	}
	cl, err := store.GetCluster(ctx, "c_001")
	if err != nil || len(cl.Members) != 2 {
		t.Fatalf("c_001 members: %v err=%v", cl.Members, err)
	}
}

func idOf(prefix string, dev, variant int) string {
	const hex = "0123456789abcdef"
	buf := []byte(prefix)
	buf = append(buf, '-', hex[dev&0xf], '-', hex[variant&0xf])
	return string(buf)
}
