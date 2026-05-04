package main

import (
	"math"
	"math/rand"
	"testing"
)

// fitPlatt 在合成 (z, y) 数据上应该收敛到能正确分类的 (A, B)。
// 数据：z 大 → y=1，z 小 → y=0；理想 (A>0, B 把分界平移到 z=0)
func TestFitPlatt_LearnsSeparator(t *testing.T) {
	z := []float64{-3, -2, -1, 1, 2, 3}
	y := []float64{0, 0, 0, 1, 1, 1}
	a, b := fitPlatt(z, y)
	// 校准后的概率应保持单调：z=3 应该 > z=-3
	highP := sigmoid(a*3 + b)
	lowP := sigmoid(a*-3 + b)
	if highP <= lowP {
		t.Fatalf("Platt should map high z → high p; got high=%.3f low=%.3f", highP, lowP)
	}
	if highP < 0.5 || lowP > 0.5 {
		t.Fatalf("Platt failed to separate: high=%.3f (want > 0.5) low=%.3f (want < 0.5)",
			highP, lowP)
	}
}

// 单类样本 → 不能拟合，返回 identity (1,0)
func TestFitPlatt_DegeneratesOnSingleClass(t *testing.T) {
	z := []float64{1, 2, 3}
	y := []float64{1, 1, 1}
	a, b := fitPlatt(z, y)
	if a != 1 || b != 0 {
		t.Fatalf("expected identity (1,0); got (%.3f, %.3f)", a, b)
	}
}

// 空输入 → identity
func TestFitPlatt_EmptyReturnsIdentity(t *testing.T) {
	a, b := fitPlatt(nil, nil)
	if a != 1 || b != 0 {
		t.Fatalf("expected (1,0); got (%.3f, %.3f)", a, b)
	}
}

// crossValidateAUC 5-fold 在线性可分数据上应该跑出高 AUC（>0.8）+ 低方差
// （std<0.2，不严苛因为 fold 间样本少）。
func TestCrossValidateAUC_LinearlySeparable(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var X [][]float64
	var y []float64
	for i := 0; i < 200; i++ {
		isFraud := rng.Float64() < 0.3
		// 一个强信号特征：fraud → 1，legit → 0（带噪音）
		f0 := 0.0
		if isFraud {
			f0 = 1.0
		}
		// 三维都加点小噪音
		X = append(X, []float64{f0, rng.Float64() * 0.2, rng.Float64() * 0.2})
		if isFraud {
			y = append(y, 1)
		} else {
			y = append(y, 0)
		}
	}
	mean, std := crossValidateAUC(X, y, 5, 50, 0.1, 0.001, []float64{1, 1}, rng)
	if mean < 0.85 {
		t.Fatalf("expected mean AUC > 0.85; got %.3f", mean)
	}
	if math.IsNaN(std) || std > 0.2 {
		t.Fatalf("expected std < 0.2; got %.3f", std)
	}
}

// k=1 / k>n / k<2 → 直接返回 (0, 0) 不 panic
func TestCrossValidateAUC_DegenerateCases(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	X := [][]float64{{1}, {0}, {1}}
	y := []float64{1, 0, 1}
	for _, k := range []int{0, 1, 100} {
		mean, std := crossValidateAUC(X, y, k, 10, 0.1, 0, []float64{1, 1}, rng)
		if mean != 0 || std != 0 {
			t.Fatalf("k=%d should return (0,0); got (%.3f,%.3f)", k, mean, std)
		}
	}
}
