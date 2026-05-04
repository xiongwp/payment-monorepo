// Package common: int64 算术溢出检查工具。
//
// 平台账户/中转账户余额可能累计到 1e15+ 量级（minor × 100，PHP 巨额累计）；
// 距 int64 上限 9.22e18 仍很安全，但**单次入参未校验**会被恶意客户端
// req.Amount=MaxInt64-1 之类的请求触发 wrap-around，把账户余额从正变负。
//
// 这些函数是 amount 进入计算前的最后防线；调用方应该已经做过业务校验
// （Amount > 0），但 defense-in-depth 也要在这里再检查一次。
package commonutil

import "math"

// AddInt64 安全加法。返回 (sum, ok)；ok=false 表示溢出（正向或负向）。
//
// 标准检测：
//
//	a > 0 && b > 0 && a > MaxInt64 - b   → 正溢出
//	a < 0 && b < 0 && a < MinInt64 - b   → 负溢出
func AddInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// SubInt64 安全减法。返回 (diff, ok)；ok=false 表示溢出。
//
// 注意：不能简单 AddInt64(a, -b)，因为 -MinInt64 在 int64 里溢出。
//
// 标准检测：
//
//	b < 0 && a > MaxInt64 + b   → 正溢出（a - 一个负数 = a + |b|）
//	b > 0 && a < MinInt64 + b   → 负溢出
func SubInt64(a, b int64) (int64, bool) {
	if b < 0 && a > math.MaxInt64+b {
		return 0, false
	}
	if b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	return a - b, true
}
