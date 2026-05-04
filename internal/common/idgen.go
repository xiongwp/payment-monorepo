package commonutil

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Snowflake constants (adapted from github.com/yongxinz/id-maker)
const (
	idWorkerBits uint8 = 10
	idNumberBits uint8 = 22

	idWorkerMax int64 = ^(-1 << idWorkerBits)
	idNumberMax int64 = ^(-1 << idNumberBits)

	idTimeShift   uint8 = idWorkerBits + idNumberBits
	idWorkerShift uint8 = idNumberBits

	idEpoch int64 = 1594364131 // fixed epoch (seconds), same as upstream
)

// IDWorker wraps snowflake to generate distributed unique IDs.
// Last 3 decimal digits encode routing: id%1000 → hundreds=dbIdx, last2=tableIdx
type IDWorker struct {
	mu        sync.Mutex
	timestamp int64
	workerID  int64
	number    int64
}

// NewIDWorker creates an IDWorker with the given workerID (0–1023).
func NewIDWorker(workerID int64) (*IDWorker, error) {
	if workerID < 0 || workerID > idWorkerMax {
		return nil, errors.New("workerID out of range [0, 1023]")
	}
	return &IDWorker{workerID: workerID}, nil
}

// NextID returns the next unique int64 ID.
// Routing can be extracted as: n = id%1000; dbIdx = n/100; tableIdx = n%100
func (w *IDWorker) NextID() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now().Unix()
	if now == w.timestamp {
		w.number++
		if w.number > idNumberMax {
			w.number = 0
			for now <= w.timestamp {
				time.Sleep(time.Millisecond) // 让出 CPU，等待时钟前进
				now = time.Now().Unix()
			}
			w.timestamp = now
		}
	} else if now > w.timestamp {
		w.number = 0
		w.timestamp = now
	} else {
		// 时钟回拨（NTP 校时 / VM 漂移）：等待时钟追上上次记录的时间戳
		for now < w.timestamp {
			time.Sleep(time.Millisecond)
			now = time.Now().Unix()
		}
		w.number++
		if w.number > idNumberMax {
			w.number = 0
			for now <= w.timestamp {
				time.Sleep(time.Millisecond)
				now = time.Now().Unix()
			}
			w.timestamp = now
		}
	}

	return (now-idEpoch)<<idTimeShift | (w.workerID << idWorkerShift) | w.number
}

// NextIDStr returns the next ID as a decimal string.
func (w *IDWorker) NextIDStr() string {
	return fmt.Sprintf("%d", w.NextID())
}

// ─── Package-level default worker (workerID=1) ───────────────────────────────

var defaultWorker *IDWorker

func init() {
	var err error
	defaultWorker, err = NewIDWorker(1)
	if err != nil {
		panic("idgen: failed to create default IDWorker: " + err.Error())
	}
}

// NextID returns the next unique ID from the package-level default worker.
func NextID() int64 {
	return defaultWorker.NextID()
}

// NextIDStr returns the next ID as a decimal string.
func NextIDStr() string {
	return defaultWorker.NextIDStr()
}
