package scaffold

import "sync/atomic"

func loadInt32(p *int32) int32   { return atomic.LoadInt32(p) }
func storeInt32(p *int32, v int32) { atomic.StoreInt32(p, v) }
