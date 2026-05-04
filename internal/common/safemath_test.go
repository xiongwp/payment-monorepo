package commonutil

import (
	"math"
	"testing"
)

func TestAddInt64_Normal(t *testing.T) {
	cases := []struct {
		a, b, want int64
	}{
		{1, 2, 3},
		{-1, -2, -3},
		{0, 0, 0},
		{math.MaxInt64 - 5, 5, math.MaxInt64},
		{math.MinInt64 + 5, -5, math.MinInt64},
	}
	for _, c := range cases {
		got, ok := AddInt64(c.a, c.b)
		if !ok {
			t.Errorf("AddInt64(%d,%d): expected ok, got overflow", c.a, c.b)
			continue
		}
		if got != c.want {
			t.Errorf("AddInt64(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAddInt64_Overflow(t *testing.T) {
	cases := []struct {
		a, b int64
	}{
		{math.MaxInt64, 1},
		{math.MaxInt64 - 5, 6},
		{math.MaxInt64, math.MaxInt64},
		{math.MinInt64, -1},
		{math.MinInt64 + 5, -6},
	}
	for _, c := range cases {
		if _, ok := AddInt64(c.a, c.b); ok {
			t.Errorf("AddInt64(%d,%d): expected overflow, got ok", c.a, c.b)
		}
	}
}

func TestSubInt64_Normal(t *testing.T) {
	cases := []struct {
		a, b, want int64
	}{
		{5, 3, 2},
		{0, 0, 0},
		{-1, -2, 1},
		{math.MaxInt64, 0, math.MaxInt64},
		{math.MinInt64, 0, math.MinInt64},
	}
	for _, c := range cases {
		got, ok := SubInt64(c.a, c.b)
		if !ok {
			t.Errorf("SubInt64(%d,%d): expected ok, got overflow", c.a, c.b)
			continue
		}
		if got != c.want {
			t.Errorf("SubInt64(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSubInt64_Overflow(t *testing.T) {
	cases := []struct {
		a, b int64
	}{
		// a - b 上溢：a 大、b 是大的负数 → a - (-x) = a + x 越界
		{math.MaxInt64, -1},
		{math.MaxInt64 - 5, -6},
		// a - b 下溢：a 小、b 是大的正数
		{math.MinInt64, 1},
		{math.MinInt64 + 5, 6},
		// 经典坑：a - MinInt64 → 数学上 a + MaxInt64 + 1 → MaxInt64 时溢出
		{0, math.MinInt64},
	}
	for _, c := range cases {
		if _, ok := SubInt64(c.a, c.b); ok {
			t.Errorf("SubInt64(%d,%d): expected overflow, got ok", c.a, c.b)
		}
	}
}
