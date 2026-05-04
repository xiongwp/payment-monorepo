package generator

import (
	"sync"
	"time"
)

const epoch = int64(1700000000000)

type Node struct {
	mu        sync.Mutex
	timestamp int64
	region    int64
	worker    int64
	seq       int64
}

func New(region, worker int64) *Node {
	return &Node{region: region, worker: worker}
}

func (n *Node) NextID() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := time.Now().UnixMilli()

	if now < n.timestamp {
		now = n.timestamp
	}

	if now == n.timestamp {
		n.seq = (n.seq + 1) & 4095
		if n.seq == 0 {
			for now <= n.timestamp {
				now = time.Now().UnixMilli()
			}
		}
	} else {
		n.seq = 0
	}

	n.timestamp = now

	return ((now - epoch) << 22) |
		(n.region << 17) |
		(n.worker << 12) |
		n.seq
}
