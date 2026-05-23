package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// fakeFleetCache 测试用，记录 Push 调用。
type fakeFleetCache struct {
	hits     map[int64]string // laID → accountNo（Get 时返回）
	pushed   map[int64][]string
	pushErr  error
}

func (f *fakeFleetCache) Get(laID int64, subIdx int) (string, bool) {
	if f.hits == nil {
		return "", false
	}
	v, ok := f.hits[laID]
	return v, ok
}
func (f *fakeFleetCache) Push(_ context.Context, laID int64, subs []string) error {
	if f.pushed == nil {
		f.pushed = make(map[int64][]string)
	}
	cp := make([]string, len(subs))
	copy(cp, subs)
	f.pushed[laID] = cp
	return f.pushErr
}

// fakeInstancesForPush 满足 AccountInstanceManager 子集
type fakeInstancesForPush struct {
	AccountInstanceManager // embed to inherit method set; other methods unused → nil panic if called
	activeFleet  []string
	listErr      error
	pushCalls    int
}

func (f *fakeInstancesForPush) ListActiveFleet(_ context.Context, _ int64) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.activeFleet, nil
}

// 验证 pushFleetCacheAfterRotation：Scheduler 拿到 fleet 后调 cache.Push
func TestScheduler_PushFleetCacheAfterRotation_HappyPath(t *testing.T) {
	cache := &fakeFleetCache{}
	inst := &fakeInstancesForPush{
		activeFleet: []string{"608054700011600047", "608054800011600048"},
	}
	s := &Scheduler{
		instances:  inst,
		fleetCache: cache,
	}

	s.pushFleetCacheAfterRotation(context.Background(), 42, zap.NewNop())

	got, ok := cache.pushed[42]
	assert.True(t, ok, "Push should be called")
	assert.Equal(t, 2, len(got))
	assert.Equal(t, "608054700011600047", got[0])
}

// fleetCache nil 时不应 panic / 不应调 instances（保留兼容性）
func TestScheduler_PushFleetCacheAfterRotation_NilCache(t *testing.T) {
	inst := &fakeInstancesForPush{activeFleet: []string{}}
	s := &Scheduler{instances: inst, fleetCache: nil}
	// 不 panic 就是成功
	s.pushFleetCacheAfterRotation(context.Background(), 1, zap.NewNop())
}

// ListActiveFleet 失败时不影响主流程（只 warn 日志）
func TestScheduler_PushFleetCacheAfterRotation_ListError(t *testing.T) {
	cache := &fakeFleetCache{}
	inst := &fakeInstancesForPush{listErr: errors.New("db down")}
	s := &Scheduler{instances: inst, fleetCache: cache}

	s.pushFleetCacheAfterRotation(context.Background(), 1, zap.NewNop())

	// Push 不应被调（因为 list 先失败）
	assert.Equal(t, 0, len(cache.pushed))
}

// Push 失败时只 warn，不抛出（保证 swap 主流程已 commit 不被回退）
func TestScheduler_PushFleetCacheAfterRotation_PushError(t *testing.T) {
	cache := &fakeFleetCache{pushErr: errors.New("config-center 502")}
	inst := &fakeInstancesForPush{activeFleet: make([]string, 100)}
	s := &Scheduler{instances: inst, fleetCache: cache}

	// 不 panic / 不 return error（pushFleetCacheAfterRotation 返回 void）
	s.pushFleetCacheAfterRotation(context.Background(), 1, zap.NewNop())
	// Push 被调了一次
	assert.Equal(t, 1, len(cache.pushed))
}

// AdminService cache 行为：cache.Get hit 时短路返回，不查 DB；
// 由于 ResolveFleetSubAccount 入口需要完整 model.LogicalAccount，
// 集成测试在 e2e 层覆盖；本文件聚焦 Scheduler.pushFleetCacheAfterRotation 逻辑。
// fleet_cache_test.go 已覆盖 cache 本身的 Get/Push 路径。
