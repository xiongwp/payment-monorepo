package metrics

import "time"

// timeNowUnixNano 单独抽出方便测试 mock (本进程内 monotonic);
// 业务代码不应直接调,用 ObserveStage 包装.
func timeNowUnixNano() int64 { return time.Now().UnixNano() }
