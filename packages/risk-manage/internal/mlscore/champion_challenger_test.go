package mlscore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixedSvc struct {
	name  string
	score float64
	err   error
	wait  time.Duration
	calls atomic.Int64
}

func (f *fixedSvc) Score(ctx context.Context, _ Features) (Result, error) {
	f.calls.Add(1)
	if f.wait > 0 {
		select {
		case <-time.After(f.wait):
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if f.err != nil {
		return Result{}, f.err
	}
	return Result{Score: f.score, ModelVer: f.name}, nil
}

func TestChampion_Score_RoutesThroughChampion(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.42}
	cc := NewChampionChallenger("v1", champ)
	r, err := cc.Score(context.Background(), Features{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Score != 0.42 || r.ModelVer != "v1" {
		t.Fatalf("expected champion score; got %+v", r)
	}
}

func TestChampion_ChallengerSideEffectFires(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)

	var mu sync.Mutex
	var got []NamedResult
	cc.SideEffect = func(_ string, _ Result, results []NamedResult) {
		mu.Lock()
		got = append([]NamedResult(nil), results...)
		mu.Unlock()
	}

	if _, err := cc.Score(context.Background(), Features{}); err != nil {
		t.Fatal(err)
	}
	// SideEffect 在 goroutine 触发，给点时间
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Name != "v2" || got[0].Score != 0.80 {
		t.Fatalf("expected challenger v2 result, got %+v", got)
	}
}

func TestChampion_ChallengerErrorDoesntAffectChampion(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.30}
	bad := &fixedSvc{name: "v2", err: errors.New("boom")}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", bad)

	var got []NamedResult
	var mu sync.Mutex
	cc.SideEffect = func(_ string, _ Result, results []NamedResult) {
		mu.Lock()
		got = append([]NamedResult(nil), results...)
		mu.Unlock()
	}
	r, err := cc.Score(context.Background(), Features{})
	if err != nil || r.Score != 0.30 {
		t.Fatalf("champion should still succeed; %+v / %v", r, err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Error == "" {
		t.Fatalf("expected challenger Error to be reported; got %+v", got)
	}
}

func TestChampion_ChallengerTimeout(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	slow := &fixedSvc{name: "v2", score: 0.80, wait: 200 * time.Millisecond}
	cc := NewChampionChallenger("v1", champ)
	cc.SetChallengerTimeout(20 * time.Millisecond)
	cc.RegisterChallenger("v2", slow)
	var got []NamedResult
	var mu sync.Mutex
	cc.SideEffect = func(_ string, _ Result, results []NamedResult) {
		mu.Lock()
		got = append([]NamedResult(nil), results...)
		mu.Unlock()
	}
	if _, err := cc.Score(context.Background(), Features{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Error == "" {
		t.Fatalf("expected timeout to surface as Error; got %+v", got)
	}
}

func TestChampion_PromoteChallenger(t *testing.T) {
	v1 := &fixedSvc{name: "v1", score: 0.10}
	v2 := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", v1)
	cc.RegisterChallenger("v2", v2)

	if !cc.PromoteChallenger("v2") {
		t.Fatal("promote should succeed")
	}
	if cc.ChampionName() != "v2" {
		t.Fatalf("champion should be v2, got %s", cc.ChampionName())
	}
	// 老 champion v1 应降级为 challenger
	chs := cc.ChallengerNames()
	if len(chs) != 1 || chs[0] != "v1" {
		t.Fatalf("expected v1 as challenger, got %v", chs)
	}
	// Score 现在走 v2
	r, _ := cc.Score(context.Background(), Features{})
	if r.Score != 0.80 {
		t.Fatalf("score should be from v2, got %v", r)
	}
}

func TestChampion_PromoteUnknownIsNoop(t *testing.T) {
	cc := NewChampionChallenger("v1", &fixedSvc{name: "v1", score: 0})
	if cc.PromoteChallenger("ghost") {
		t.Fatal("promote unknown should fail")
	}
	if cc.ChampionName() != "v1" {
		t.Fatal("champion should still be v1")
	}
}

func TestChampion_RegisterReplacesByName(t *testing.T) {
	cc := NewChampionChallenger("v1", &fixedSvc{name: "v1", score: 0})
	cc.RegisterChallenger("v2", &fixedSvc{name: "v2", score: 0.1})
	cc.RegisterChallenger("v2", &fixedSvc{name: "v2", score: 0.9})
	if len(cc.ChallengerNames()) != 1 {
		t.Fatalf("expected 1 challenger after replace, got %v", cc.ChallengerNames())
	}
	var got []NamedResult
	var mu sync.Mutex
	cc.SideEffect = func(_ string, _ Result, results []NamedResult) {
		mu.Lock()
		got = results
		mu.Unlock()
	}
	cc.Score(context.Background(), Features{})
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Score != 0.9 {
		t.Fatalf("expected replaced challenger score 0.9, got %+v", got)
	}
}

func TestRemoveChallenger(t *testing.T) {
	champ := NewLogisticService()
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", champ)
	cc.RegisterChallenger("v3", champ)
	if !cc.RemoveChallenger("v2") {
		t.Fatal("RemoveChallenger(v2) should return true")
	}
	if cc.RemoveChallenger("v2") {
		t.Fatal("removing missing challenger should return false")
	}
	names := cc.ChallengerNames()
	if len(names) != 1 || names[0] != "v3" {
		t.Fatalf("expected [v3] remaining; got %+v", names)
	}
	if cc.ChampionName() != "v1" {
		t.Fatal("champion should be unchanged after challenger removal")
	}
}
