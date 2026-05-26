// nebula_linkstore_test.go: 真集成测试，需要本地 / CI 跑着 NebulaGraph。
//
// 跑法：
//
//	export NEBULA_TEST_ADDR=127.0.0.1:9669
//	export NEBULA_TEST_USER=root
//	export NEBULA_TEST_PASS=nebula
//	export NEBULA_TEST_SPACE=risk_graph_test
//	go test -tags nebula -run TestNebula ./internal/store/...
//
// 无 NEBULA_TEST_ADDR 时全部 Skip。schema 初始化用 deploy/nebulagraph/schema.ngql。
// 每个测试用一个独立 vid 前缀，避免互相干扰；测试结尾 best-effort Purge。

//go:build nebula

package store

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// testNebula 拿 env 配置；不全则 Skip。
func testNebula(t *testing.T) (*NebulaLinkStore, func()) {
	t.Helper()
	addr := os.Getenv("NEBULA_TEST_ADDR")
	if addr == "" {
		t.Skip("set NEBULA_TEST_ADDR=host:9669 to run nebula integration tests")
	}
	user := os.Getenv("NEBULA_TEST_USER")
	if user == "" {
		user = "root"
	}
	pass := os.Getenv("NEBULA_TEST_PASS")
	if pass == "" {
		pass = "nebula"
	}
	space := os.Getenv("NEBULA_TEST_SPACE")
	if space == "" {
		space = "risk_graph_test"
	}
	logger, _ := zap.NewDevelopment()
	s, err := NewNebulaLinkStoreFromAddrs([]string{addr}, user, pass, space, logger)
	if err != nil {
		t.Fatalf("nebula init: %v", err)
	}
	return s, func() { s.Close() }
}

// nsKey 给当前测试加唯一前缀 —— 避免并发跑测试时互相踩。
func nsKey(t *testing.T, suffix string) string {
	return fmt.Sprintf("nbtest_%s_%d:%s", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano(), suffix)
}

func TestNebulaLinkStore_LinkPeers(t *testing.T) {
	s, done := testNebula(t)
	defer done()
	ctx := context.Background()

	dev := nsKey(t, "device_abc")
	cust := nsKey(t, "customer_42")
	defer s.Purge(ctx, dev)
	defer s.Purge(ctx, cust)

	s.Link(ctx, dev, cust)
	// nebula 写后 read 通常立即可见（同 leader），偶发场景小睡一下兜底
	time.Sleep(50 * time.Millisecond)

	got := s.Peers(ctx, dev, "")
	if len(got) != 1 || got[0] != cust {
		t.Fatalf("device→customer peers: got %v want [%s]", got, cust)
	}
	gotRev := s.Peers(ctx, cust, "")
	if len(gotRev) != 1 || gotRev[0] != dev {
		t.Fatalf("customer→device peers (bidirectional): got %v want [%s]", gotRev, dev)
	}
}

func TestNebulaLinkStore_PrefixFilter(t *testing.T) {
	s, done := testNebula(t)
	defer done()
	ctx := context.Background()

	dev := nsKey(t, "device_x")
	cust := nsKey(t, "customer_1")
	ipKey := nsKey(t, "ip_1.2.3.4")
	defer s.Purge(ctx, dev)
	defer s.Purge(ctx, cust)
	defer s.Purge(ctx, ipKey)

	s.Link(ctx, dev, cust)
	s.Link(ctx, dev, ipKey)
	time.Sleep(50 * time.Millisecond)

	all := s.Peers(ctx, dev, "")
	if len(all) != 2 {
		t.Fatalf("no-prefix peers: got %d want 2 (%v)", len(all), all)
	}
	// peerPrefix 用动态前缀（不是 "customer:" 因为 nsKey 加了时间戳前缀）
	cprefix := strings.SplitN(cust, ":", 2)[0] + ":"
	cs := s.Peers(ctx, dev, cprefix)
	if len(cs) != 1 || cs[0] != cust {
		t.Fatalf("customer prefix filter: got %v want [%s]", cs, cust)
	}
}

func TestNebulaLinkStore_PeersWithin2Hop(t *testing.T) {
	s, done := testNebula(t)
	defer done()
	ctx := context.Background()

	a := nsKey(t, "a")
	b := nsKey(t, "b")
	c := nsKey(t, "c")
	defer s.Purge(ctx, a)
	defer s.Purge(ctx, b)
	defer s.Purge(ctx, c)

	s.Link(ctx, a, b)
	s.Link(ctx, b, c)
	time.Sleep(50 * time.Millisecond)

	// a → b (1-hop), b → c (1-hop) ⇒ a 的 2-hop 邻居含 b, c
	within := s.PeersWithin(ctx, a, 2, "")
	has := map[string]bool{}
	for _, k := range within {
		has[k] = true
	}
	if !has[b] || !has[c] {
		t.Fatalf("PeersWithin(a, 2): missing b/c, got %v", within)
	}
	if has[a] {
		t.Fatal("PeersWithin should exclude origin")
	}
}

func TestNebulaLinkStore_TagsWithin(t *testing.T) {
	s, done := testNebula(t)
	defer done()
	ctx := context.Background()

	a := nsKey(t, "a")
	b := nsKey(t, "b")
	defer s.Purge(ctx, a)
	defer s.Purge(ctx, b)

	s.Link(ctx, a, b)
	s.Tag(ctx, b, "fraud")
	time.Sleep(80 * time.Millisecond) // tag 索引同步通常 < 50ms，给 80ms buffer

	tags := s.TagsWithin(ctx, a, 1)
	if tags["fraud"] < 1 {
		t.Fatalf("TagsWithin should pick up fraud tag on neighbor: got %v", tags)
	}
}

// TestNebulaLinkStore_WeightedFanout_Decay inject 三条边 last_observed
// 分别 = now-50min / now-30min / now-5min，halflife=10min，验证 weight 单调
// 且数值 ≈ e^(-Δt/halflife)。
//
// 因为 nebula INSERT EDGE 直接接受 timestamp 数值，我们绕开 Link() 自己手工
// 写一条 INSERT EDGE 把 `at` 调到过去 —— 类似 mem_linkstore 的 injectEdge。
func TestNebulaLinkStore_WeightedFanout_Decay(t *testing.T) {
	s, done := testNebula(t)
	defer done()
	ctx := context.Background()

	src := nsKey(t, "src")
	peers := []string{nsKey(t, "peer_50min"), nsKey(t, "peer_30min"), nsKey(t, "peer_5min")}
	defer s.Purge(ctx, src)
	for _, p := range peers {
		defer s.Purge(ctx, p)
	}

	// 手工 inject：UPSERT VERTEX + INSERT EDGE with at=<past unix>
	ages := []time.Duration{50 * time.Minute, 30 * time.Minute, 5 * time.Minute}
	now := time.Now()
	srcVid := vidOf(src)
	exp := now.Add(linkTTL).Unix()
	for i, p := range peers {
		pVid := vidOf(p)
		at := now.Add(-ages[i]).Unix()
		q := fmt.Sprintf(
			`UPSERT VERTEX ON identity "%s" SET key = "%s";
			 UPSERT VERTEX ON identity "%s" SET key = "%s";
			 INSERT EDGE links(at, expires) VALUES "%s" -> "%s": (%d, %d), "%s" -> "%s": (%d, %d);`,
			srcVid, escapeStr(src), pVid, escapeStr(p),
			srcVid, pVid, at, exp,
			pVid, srcVid, at, exp)
		if _, err := s.execute(ctx, q); err != nil {
			t.Fatalf("inject %s: %v", p, err)
		}
	}
	time.Sleep(80 * time.Millisecond)

	halflife := 10 * time.Minute
	totalW, count := s.WeightedFanout(ctx, src, "", halflife)
	if count != 3 {
		t.Fatalf("WeightedFanout count: got %d want 3", count)
	}
	// 期望 ≈ e^(-50/10) + e^(-30/10) + e^(-5/10) = 0.00674 + 0.0498 + 0.6065 ≈ 0.663
	expected := math.Exp(-5) + math.Exp(-3) + math.Exp(-0.5)
	// 容忍 ±20%（时间戳粒度 = 秒；linkTTL 兜底；nebula 时钟漂移）
	if math.Abs(totalW-expected)/expected > 0.20 {
		t.Fatalf("WeightedFanout decay: got %.4f want ≈ %.4f (±20%%)", totalW, expected)
	}

	// 0 halflife 应该退化成 unweighted （= count）
	uw, _ := s.WeightedFanout(ctx, src, "", 0)
	if math.Abs(uw-3.0) > 0.01 {
		t.Fatalf("unweighted fallback: got %.4f want 3.0", uw)
	}
}

// TestNebulaLinkStore_Healthcheck 起 store 后立即查 Healthy() 应该 true；
// 短暂等待让 ticker 跑一轮（实际 30s 间隔太长，这里只验初始态）。
func TestNebulaLinkStore_Healthcheck(t *testing.T) {
	s, done := testNebula(t)
	defer done()
	if !s.Healthy() {
		t.Fatal("expected Healthy() == true right after init")
	}
}

// TestNebulaLinkStore_Purge 写完再 Purge 应该清掉所有出边。
func TestNebulaLinkStore_Purge(t *testing.T) {
	s, done := testNebula(t)
	defer done()
	ctx := context.Background()

	a := nsKey(t, "a")
	b := nsKey(t, "b")
	c := nsKey(t, "c")
	defer s.Purge(ctx, b)
	defer s.Purge(ctx, c)

	s.Link(ctx, a, b)
	s.Link(ctx, a, c)
	time.Sleep(50 * time.Millisecond)

	if peers := s.Peers(ctx, a, ""); len(peers) != 2 {
		t.Fatalf("before purge: got %d peers", len(peers))
	}
	n := s.Purge(ctx, a)
	if n <= 0 {
		t.Fatalf("Purge should report >0 edges removed, got %d", n)
	}
	time.Sleep(80 * time.Millisecond)
	if peers := s.Peers(ctx, a, ""); len(peers) != 0 {
		t.Fatalf("after purge: expected 0 peers, got %d", len(peers))
	}
}

