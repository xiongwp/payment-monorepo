package serviceregistry

import (
	"context"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// RunLeaderLoop campaigns for leadership at `key`, runs `task` while elected,
// then re-campaigns on session loss. Blocks until ctx is cancelled.
//
// Behaviour:
//   - cli == nil → task(ctx) runs once with no election (dev / single-pod mode).
//   - On Campaign error → 5s backoff then retry. ctx-cancel breaks out.
//   - While leader → task receives a derived context that's cancelled when
//     the underlying etcd session is invalidated (etcd disconnect, lease lost).
//     Task should exit cleanly when its ctx is done; loop then re-campaigns.
//
// `identity` is the value written under the leader key; pick a per-pod
// identifier (hostname, POD_NAME) so `etcdctl get` shows who's leading.
func RunLeaderLoop(ctx context.Context, cli *clientv3.Client, key, identity string, ttl time.Duration, task func(ctx context.Context)) {
	if cli == nil {
		// 没接 etcd：单 pod / dev 模式。直接跑 task，由 ctx 控制退出。
		task(ctx)
		return
	}
	if ttl < time.Second {
		ttl = 10 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		el, err := NewElection(cli, key, ttl)
		if err != nil {
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		if err := el.Campaign(ctx, identity); err != nil {
			_ = el.Close()
			if ctx.Err() != nil {
				return
			}
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}

		// 当选 leader：派生一个子 ctx，sessionLost 或 ctx 取消都会 cancel 它。
		leaderCtx, leaderCancel := context.WithCancel(ctx)
		sessDone := el.Done()
		go func() {
			select {
			case <-sessDone:
				leaderCancel()
			case <-leaderCtx.Done():
			}
		}()
		task(leaderCtx)
		leaderCancel()
		_ = el.Close()
		// 循环回去重新 NewElection / Campaign。
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
