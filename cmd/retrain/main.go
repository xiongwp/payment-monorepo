// cmd/retrain —— 离线训练 logistic regression，输出 LogisticConfig JSON。
//
// 用法：
//
//   # 从 ml.feature_snapshot stream（join 过 outcome）拉到 JSONL，喂进来
//   cat snapshots.jsonl | ./retrain --epochs 100 --lr 0.1 > model_v2.json
//
//   # 落地后 admin reload：把 model_v2.json 灌到 admin /config/ml-config 端点
//   # （main.go 起来用 viper 装载到 mlscore.LogisticService）
//
// 算法：标准 batch logistic regression + L2 正则 + 梯度下降。
// 对接 mlscore.LogisticService 的 FeatureWeights 字段一致；训练完直接
// 输出 LogisticConfig{ModelVer, Intercept, Weights} 可灌进现有 service。
//
// 不依赖外部 ML 库（sklearn / TF）；纯 Go + math，单文件可读，方便商业部署
// 在没有 python 工具链的环境跑。生产规模数据建议导出到 sklearn 训练（更快
// + 更优 solver），本工具是 baseline + 单测 + on-call quick-fix 用。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"

	"github.com/xiongwp/risk-manage/internal/featurestore"
	"github.com/xiongwp/risk-manage/internal/mlscore"
)

func main() {
	epochs := flag.Int("epochs", 100, "gradient descent 迭代次数")
	lr := flag.Float64("lr", 0.1, "学习率")
	l2 := flag.Float64("l2", 0.001, "L2 正则系数")
	modelVer := flag.String("model-ver", "logistic-v2", "输出模型版本号")
	testRatio := flag.Float64("test-ratio", 0.2, "hold-out test set 占比 (0=disable)")
	classWeight := flag.Bool("class-weight", true,
		"fraud 类自动按 1/freq 加权，对抗严重不均衡（典型场景 fraud<5%）")
	seed := flag.Int64("seed", 42, "random seed for shuffle / split")
	cvFolds := flag.Int("cv-folds", 0,
		"k-fold cross-validation 折数 (0=disable，5=典型 5-fold)，开启后报告 AUC mean/std")
	calibrate := flag.Bool("calibrate", true,
		"训练完跑 Platt scaling 校准概率（class_weight 模式下尤其需要）")
	flag.Parse()

	X, y, err := readSnapshots(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read snapshots failed: %v\n", err)
		os.Exit(1)
	}
	if len(X) == 0 {
		fmt.Fprintln(os.Stderr, "no snapshots with outcome label found")
		os.Exit(1)
	}
	posCount := 0
	for _, v := range y {
		if v == 1 {
			posCount++
		}
	}
	fmt.Fprintf(os.Stderr, "loaded %d samples (%d fraud=%.2f%%, %d legit), %d features\n",
		len(X), posCount, 100*float64(posCount)/float64(len(X)),
		len(X)-posCount, len(X[0]))

	// shuffle + train/test split
	rng := rand.New(rand.NewSource(*seed))
	idx := rng.Perm(len(X))
	splitAt := len(X)
	if *testRatio > 0 && *testRatio < 1 {
		splitAt = int(float64(len(X)) * (1 - *testRatio))
	}
	trainX, trainY := make([][]float64, splitAt), make([]float64, splitAt)
	for i, ii := range idx[:splitAt] {
		trainX[i], trainY[i] = X[ii], y[ii]
	}
	testX, testY := make([][]float64, len(X)-splitAt), make([]float64, len(X)-splitAt)
	for i, ii := range idx[splitAt:] {
		testX[i], testY[i] = X[ii], y[ii]
	}
	fmt.Fprintf(os.Stderr, "split: train=%d test=%d\n", len(trainX), len(testX))

	// class weight：fraud 1/p_pos，legit 1/p_neg；归一化避免 lr 失衡
	weightsByLabel := []float64{1, 1}
	if *classWeight && posCount > 0 && posCount < len(X) {
		nPos := float64(posCount)
		nNeg := float64(len(X) - posCount)
		// 反频率，避免极端样本（n=1）
		weightsByLabel[0] = (nPos + nNeg) / (2 * nNeg)
		weightsByLabel[1] = (nPos + nNeg) / (2 * nPos)
		fmt.Fprintf(os.Stderr, "class_weight: legit=%.3f fraud=%.3f\n",
			weightsByLabel[0], weightsByLabel[1])
	}

	// 可选：k-fold CV 跑稳定性评估（在 final fit 之前，独立于 train/test split）。
	// 报告 K 个 fold 的 AUC mean / std-dev，让运营判断模型稳定性 — std > 0.05
	// 说明数据噪声大，不该升级模型。
	if *cvFolds > 1 && *cvFolds <= len(X) {
		mean, std := crossValidateAUC(X, y, *cvFolds, *epochs, *lr, *l2, weightsByLabel, rng)
		fmt.Fprintf(os.Stderr, "\n=== %d-fold CV ===\n AUC mean=%.4f std=%.4f\n",
			*cvFolds, mean, std)
		if std > 0.05 {
			fmt.Fprintf(os.Stderr, " WARN: std > 0.05 表示 fold 间方差大，模型可能不稳定，建议增加样本或简化特征\n")
		}
	}

	intercept, weights := trainWeighted(trainX, trainY, *epochs, *lr, *l2, weightsByLabel)

	cfg := mlscore.LogisticConfig{
		ModelVer:  *modelVer,
		Intercept: intercept,
		Weights:   weightsToFeatureWeights(weights),
	}

	// Platt scaling：在 hold-out test 上拟合 sigmoid(A*z + B) → 真实 label。
	// 修正 class_weight 引入的概率偏置，让 0.5 阈值真的对应"50% 欺诈概率"。
	// 没有 hold-out（test_ratio=0）→ 跳过；len(test)<10 → 跳过避免过拟合。
	if *calibrate && len(testX) >= 10 {
		rawZ := decisionAll(testX, intercept, weights)
		a, b := fitPlatt(rawZ, testY)
		cfg.PlattA = a
		cfg.PlattB = b
		fmt.Fprintf(os.Stderr, "\n=== Platt calibration ===\n A=%.4f B=%.4f (sigmoid(A*z+B) → calibrated p)\n", a, b)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "encode config: %v\n", err)
		os.Exit(1)
	}

	// 训练集 baseline
	trainLoss, trainAcc := evaluate(trainX, trainY, intercept, weights)
	fmt.Fprintf(os.Stderr, "\n=== train ===\n loss=%.4f acc=%.4f\n", trainLoss, trainAcc)

	// 测试集（如果有）— ROC-AUC + threshold-sweep 找最佳 F1
	if len(testX) > 0 {
		testLoss, testAcc := evaluate(testX, testY, intercept, weights)
		probs := scoreAll(testX, intercept, weights)
		auc := rocAUC(probs, testY)
		bestT, bestF1, bestPrec, bestRec := bestThreshold(probs, testY)
		fmt.Fprintf(os.Stderr, "\n=== test (held-out %d samples) ===\n", len(testX))
		fmt.Fprintf(os.Stderr, " loss=%.4f acc=%.4f auc=%.4f\n", testLoss, testAcc, auc)
		fmt.Fprintf(os.Stderr, " best threshold=%.3f → precision=%.4f recall=%.4f f1=%.4f\n",
			bestT, bestPrec, bestRec, bestF1)
		// 标准 0.5 阈值的 P/R 也打印（生产部署用 ml_threshold 规则跟它对比）
		p50, r50, f50 := prAtThreshold(probs, testY, 0.5)
		fmt.Fprintf(os.Stderr, " @0.5         → precision=%.4f recall=%.4f f1=%.4f\n",
			p50, r50, f50)
	}

	// feature importance：按 |weight| 排序，让运维一眼看出哪些特征驱动了模型
	fmt.Fprintln(os.Stderr, "\n=== feature importance (|weight| desc) ===")
	for _, fi := range featureImportance(weights) {
		fmt.Fprintf(os.Stderr, " %-22s %+.4f\n", fi.name, fi.weight)
	}
}

// readSnapshots 从 stdin 读 JSONL 格式 featurestore.Snapshot。
// 只保留 outcome_label != nil 的样本（label 必须才能监督学习）。
func readSnapshots(r io.Reader) ([][]float64, []float64, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<24) // up to 16MB lines
	var X [][]float64
	var y []float64
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var snap featurestore.Snapshot
		if err := json.Unmarshal([]byte(line), &snap); err != nil {
			fmt.Fprintf(os.Stderr, "skipping bad json line: %v\n", err)
			continue
		}
		if snap.OutcomeLabel == nil || snap.Features == nil {
			continue
		}
		X = append(X, featuresToVector(snap.Features))
		if *snap.OutcomeLabel {
			y = append(y, 1.0)
		} else {
			y = append(y, 0.0)
		}
	}
	return X, y, scanner.Err()
}

// featuresToVector 把 Features → 固定顺序二值化向量。
// 顺序必须跟 weightsToFeatureWeights 反向解构一致。
// 跟 mlscore.LogisticService.Score 内部的 binarize 逻辑保持一致，方便重训完
// 直接灌进现有 service。
func featuresToVector(f *mlscore.Features) []float64 {
	return []float64{
		boolFloat(f.Amount > 10_000_000),                                  // HighAmount: > ¥100k
		boolFloat(f.IPProxy),                                              // IPProxy
		boolFloat(f.IPVPN),                                                // IPVPN
		boolFloat(f.IPDataCenter),                                         // IPDataCenter
		boolFloat(f.IPCountry != "" && f.Country != "" && f.IPCountry != f.Country), // IPCountryMismatch
		boolFloat(f.FingerprintHash == ""),                                // NoFingerprint
		boolFloat(strings.Contains(strings.ToLower(f.WebGLRenderer), "swiftshader") ||
			strings.Contains(strings.ToLower(f.WebGLRenderer), "llvmpipe")), // HeadlessRenderer
		boolFloat(f.HardwareConcurrency <= 1),                            // LowConcurrency
		boolFloat(f.TimeToCheckoutMs > 0 && f.TimeToCheckoutMs < 2000),   // RapidCheckout
		boolFloat(f.MouseMovementEntropy == 0),                           // NoMouseEntropy
		boolFloat(f.TypingRhythmCV > 0 && f.TypingRhythmCV < 0.05),       // BotTypingRhythm
		boolFloat(f.KeystrokeCount == 0),                                 // NoKeystrokes
		boolFloat(isHighRiskCountry(f.Country)),                          // HighRiskCountry
	}
}

// weightsToFeatureWeights 反向：训出来的权重 → FeatureWeights struct。
// 顺序必须跟 featuresToVector 一致。
func weightsToFeatureWeights(w []float64) mlscore.FeatureWeights {
	get := func(i int) float64 {
		if i < len(w) {
			return w[i]
		}
		return 0
	}
	return mlscore.FeatureWeights{
		HighAmount:        get(0),
		IPProxy:           get(1),
		IPVPN:             get(2),
		IPDataCenter:      get(3),
		IPCountryMismatch: get(4),
		NoFingerprint:     get(5),
		HeadlessRenderer:  get(6),
		LowConcurrency:    get(7),
		RapidCheckout:     get(8),
		NoMouseEntropy:    get(9),
		BotTypingRhythm:   get(10),
		NoKeystrokes:      get(11),
		HighRiskCountry:   get(12),
	}
}

// isHighRiskCountry 跟 mlscore.LogisticService 保持一致即可；这里给极简列表。
// 真实生产应从配置加载。
func isHighRiskCountry(iso string) bool {
	switch strings.ToUpper(iso) {
	case "NG", "VE", "RU", "BY", "KP", "IR":
		return true
	}
	return false
}

// train batch GD + L2。返回 (intercept, weights)。
func train(X [][]float64, y []float64, epochs int, lr, l2 float64) (float64, []float64) {
	n := len(X)
	d := len(X[0])
	w := make([]float64, d)
	b := 0.0
	for ep := 0; ep < epochs; ep++ {
		var gradB float64
		gradW := make([]float64, d)
		for i := 0; i < n; i++ {
			z := b
			for j := 0; j < d; j++ {
				z += w[j] * X[i][j]
			}
			p := sigmoid(z)
			diff := p - y[i]
			gradB += diff
			for j := 0; j < d; j++ {
				gradW[j] += diff * X[i][j]
			}
		}
		// 平均梯度 + L2
		fn := float64(n)
		b -= lr * gradB / fn
		for j := 0; j < d; j++ {
			w[j] -= lr * (gradW[j]/fn + l2*w[j])
		}
	}
	return b, w
}

func evaluate(X [][]float64, y []float64, b float64, w []float64) (loss, acc float64) {
	correct := 0
	for i := range X {
		z := b
		for j := range w {
			z += w[j] * X[i][j]
		}
		p := sigmoid(z)
		// 二元交叉熵
		const eps = 1e-9
		if y[i] == 1 {
			loss += -math.Log(math.Max(p, eps))
		} else {
			loss += -math.Log(math.Max(1-p, eps))
		}
		pred := 0.0
		if p >= 0.5 {
			pred = 1.0
		}
		if pred == y[i] {
			correct++
		}
	}
	loss /= float64(len(X))
	acc = float64(correct) / float64(len(X))
	return
}

func sigmoid(x float64) float64 { return 1.0 / (1.0 + math.Exp(-x)) }
func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// trainWeighted batch GD + L2，每条样本按其类别权重加权（class imbalance 处理）。
// classWeights[0]=负类权重，[1]=正类权重。weights=[1,1] 时退到普通无加权 train。
func trainWeighted(X [][]float64, y []float64, epochs int, lr, l2 float64, classWeights []float64) (float64, []float64) {
	n := len(X)
	if n == 0 {
		return 0, nil
	}
	d := len(X[0])
	w := make([]float64, d)
	b := 0.0
	for ep := 0; ep < epochs; ep++ {
		var gradB float64
		gradW := make([]float64, d)
		var totalW float64
		for i := 0; i < n; i++ {
			z := b
			for j := 0; j < d; j++ {
				z += w[j] * X[i][j]
			}
			p := sigmoid(z)
			cw := classWeights[0]
			if y[i] == 1 {
				cw = classWeights[1]
			}
			diff := (p - y[i]) * cw
			gradB += diff
			for j := 0; j < d; j++ {
				gradW[j] += diff * X[i][j]
			}
			totalW += cw
		}
		if totalW <= 0 {
			totalW = float64(n)
		}
		b -= lr * gradB / totalW
		for j := 0; j < d; j++ {
			w[j] -= lr * (gradW[j]/totalW + l2*w[j])
		}
	}
	return b, w
}

// scoreAll 跑完一遍所有样本的 sigmoid score，给 ROC-AUC / threshold-sweep 用。
func scoreAll(X [][]float64, b float64, w []float64) []float64 {
	out := make([]float64, len(X))
	for i := range X {
		z := b
		for j := range w {
			z += w[j] * X[i][j]
		}
		out[i] = sigmoid(z)
	}
	return out
}

// decisionAll 跟 scoreAll 一致但返回 raw decision z（未过 sigmoid）。
// 给 Platt scaling 拟合输入用。
func decisionAll(X [][]float64, b float64, w []float64) []float64 {
	out := make([]float64, len(X))
	for i := range X {
		z := b
		for j := range w {
			z += w[j] * X[i][j]
		}
		out[i] = z
	}
	return out
}

// fitPlatt 在 (z, y) 对上拟合 sigmoid(A*z + B) 把 raw decision 映射成真实概率。
// 用 batch GD 跑 100 epoch 学 (A, B) 二维参数，标准 binary cross-entropy 损失。
//
// 用 Platt 原始论文里推荐的 label smoothing：
//
//	t_pos = (n_pos + 1) / (n_pos + 2)
//	t_neg = 1 / (n_neg + 2)
//
// 防止过拟合 + 让数学上 likelihood 永远 > 0。
//
// 输出 (A, B) 写到 LogisticConfig.PlattA / PlattB；零值 → 走原 sigmoid 路径。
func fitPlatt(z, y []float64) (a, b float64) {
	if len(z) == 0 {
		return 1, 0 // 退化：identity 映射
	}
	var nPos, nNeg int
	for _, v := range y {
		if v == 1 {
			nPos++
		} else {
			nNeg++
		}
	}
	if nPos == 0 || nNeg == 0 {
		// 单类样本无法拟合校准；返回 identity
		return 1, 0
	}
	tPos := (float64(nPos) + 1) / (float64(nPos) + 2)
	tNeg := 1.0 / (float64(nNeg) + 2)

	// 初值：A=1, B=0（等价 identity）。100 epoch + lr=0.1 + L2 几乎不需要
	// 因为只有 2 个参数。
	a, b = 1.0, 0.0
	const epochs = 100
	const lr = 0.1
	for ep := 0; ep < epochs; ep++ {
		var gA, gB float64
		for i, zi := range z {
			t := tNeg
			if y[i] == 1 {
				t = tPos
			}
			p := sigmoid(a*zi + b)
			diff := p - t
			gA += diff * zi
			gB += diff
		}
		n := float64(len(z))
		a -= lr * gA / n
		b -= lr * gB / n
	}
	return a, b
}

// crossValidateAUC 跑 k-fold CV，返回每折 AUC 的均值 / 标准差。
// rng 决定 fold split 顺序（用入参 rng 让 cmd 主路径的 seed 也覆盖到 CV）。
func crossValidateAUC(X [][]float64, y []float64, k, epochs int, lr, l2 float64, classWeights []float64, rng *rand.Rand) (mean, std float64) {
	n := len(X)
	if k < 2 || k > n {
		return 0, 0
	}
	idx := rng.Perm(n)
	foldSize := n / k
	aucs := make([]float64, 0, k)
	for f := 0; f < k; f++ {
		valStart := f * foldSize
		valEnd := valStart + foldSize
		if f == k-1 {
			valEnd = n // 最后一折吃掉剩余尾巴（若 n 不能整除 k）
		}
		var trainX [][]float64
		var trainY []float64
		var valX [][]float64
		var valY []float64
		for i, ii := range idx {
			if i >= valStart && i < valEnd {
				valX = append(valX, X[ii])
				valY = append(valY, y[ii])
			} else {
				trainX = append(trainX, X[ii])
				trainY = append(trainY, y[ii])
			}
		}
		intercept, weights := trainWeighted(trainX, trainY, epochs, lr, l2, classWeights)
		probs := scoreAll(valX, intercept, weights)
		aucs = append(aucs, rocAUC(probs, valY))
	}
	for _, a := range aucs {
		mean += a
	}
	mean /= float64(len(aucs))
	for _, a := range aucs {
		std += (a - mean) * (a - mean)
	}
	std = math.Sqrt(std / float64(len(aucs)))
	return mean, std
}

// rocAUC 用经典 Mann-Whitney U 等价公式：把所有 (pos, neg) 对里 prob_pos > prob_neg
// 的占比就是 AUC。tied 算半个。O(N log N) — 排序 + 一次扫。
//
// AUC ∈ [0.5, 1.0]：0.5 = 随机；> 0.7 可用；> 0.85 优秀；< 0.5 模型反着接（bug）。
func rocAUC(probs []float64, y []float64) float64 {
	type pair struct {
		prob  float64
		label float64
	}
	pairs := make([]pair, len(probs))
	for i := range probs {
		pairs[i] = pair{probs[i], y[i]}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].prob < pairs[j].prob })
	var posRanks float64
	nPos, nNeg := 0, 0
	for i, p := range pairs {
		// rank 从 1 开始；同 prob 按 average rank 处理（简化：直接用 i+1）
		if p.label == 1 {
			posRanks += float64(i + 1)
			nPos++
		} else {
			nNeg++
		}
	}
	if nPos == 0 || nNeg == 0 {
		return 0.5
	}
	auc := (posRanks - float64(nPos)*float64(nPos+1)/2) / (float64(nPos) * float64(nNeg))
	return auc
}

// prAtThreshold 给定阈值算 precision / recall / F1。
func prAtThreshold(probs, y []float64, threshold float64) (precision, recall, f1 float64) {
	tp, fp, fn := 0, 0, 0
	for i, p := range probs {
		pred := 0.0
		if p >= threshold {
			pred = 1
		}
		switch {
		case pred == 1 && y[i] == 1:
			tp++
		case pred == 1 && y[i] == 0:
			fp++
		case pred == 0 && y[i] == 1:
			fn++
		}
	}
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp+fn)
	}
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	return
}

// bestThreshold 在 [0.05, 0.95] 步长 0.01 sweep 找 F1 最大的阈值。
// 给运营 ml_threshold 规则的 .threshold 配值参考。
func bestThreshold(probs, y []float64) (best, bestF1, bestP, bestR float64) {
	best = 0.5
	for t := 0.05; t <= 0.95; t += 0.01 {
		p, r, f := prAtThreshold(probs, y, t)
		if f > bestF1 {
			bestF1 = f
			bestP = p
			bestR = r
			best = t
		}
	}
	return
}

// featureImportance 把训练出的 weights 跟特征名对齐，按 |w| 倒序。
type featureScore struct {
	name   string
	weight float64
}

func featureImportance(w []float64) []featureScore {
	names := []string{
		"HighAmount", "IPProxy", "IPVPN", "IPDataCenter", "IPCountryMismatch",
		"NoFingerprint", "HeadlessRenderer", "LowConcurrency", "RapidCheckout",
		"NoMouseEntropy", "BotTypingRhythm", "NoKeystrokes", "HighRiskCountry",
	}
	out := make([]featureScore, 0, len(w))
	for i, v := range w {
		n := fmt.Sprintf("f%d", i)
		if i < len(names) {
			n = names[i]
		}
		out = append(out, featureScore{n, v})
	}
	sort.Slice(out, func(i, j int) bool {
		return math.Abs(out[i].weight) > math.Abs(out[j].weight)
	})
	return out
}
