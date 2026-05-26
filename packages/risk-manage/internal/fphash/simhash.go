// Package fphash 提供 64-bit SimHash 模糊指纹匹配。
//
// 背景：风控引擎用 sha256(canvas+webgl+audio+font+...) 做精确设备识别。
// 浏览器小版本升级（Chrome 121 → 122）会让 canvas 渲染像素差一两个，
// audioContext 量化噪声变化，FontHash 多/少一个新装的字体 — 整个 sha256
// mismatch，一台设备在升级前后被识别为 2 台。SimHash 通过对每个 feature piece
// 独立 hash + bit-wise 投票，让"少数信号变化"只产生几个 bit 翻转，hamming
// 距离 < 8 视为同设备。
//
// 算法（Charikar 2002，64-bit 版）：
//
//  1. 对每个 feature piece（"canvas:abc123" / "webgl:Intel UHD" / "audio:0.013"）
//     计算 64-bit fnv-1a hash
//  2. 维护 64 维 int 累加器；对每个 hash 的每个 bit：bit=1 → +1，bit=0 → -1
//  3. 最终累加器 > 0 → SimHash 对应 bit = 1，否则 = 0
//
// 性能：N 个 piece 各 O(64)，1 个指纹 < 5μs。完全纯计算 + 无外部依赖。
//
// 不做的：LSH（locality-sensitive hashing）索引。当前 PeersBySimHash 用线性扫，
// 1M device 量级单查 < 20ms 可接受；超过则换 LSH bucket（典型 4-band × 16-bit
// prefix）。详见 LinkStore.PeersBySimHash 注释里的 TODO。
package fphash

import (
	"hash/fnv"
	"math/bits"
)

// DefaultMatchThreshold 默认匹配阈值（汉明距离 <= 8 视为同设备）。
//
// 经验值：64-bit SimHash 完全随机指纹间距离均值 ~32，方差 ~16；同源指纹
// "少改 1-2 个 piece"距离一般 < 4，"小版本升级 / 字体微变"距离 4-8，
// 不同设备距离 > 12。8 是 fraud detection 经验上 false-positive 与
// false-negative 的折中（业界 Stripe/Sift/Forter 类似 6-10）。
const DefaultMatchThreshold = 8

// SimHash 对 feature pieces 计算 64-bit SimHash。
//
// pieces 顺序无关（实现是 commutative 累加）；空 slice / 全空字符串 → 0。
// 单个 piece 建议格式 "<dim>:<value>"（"canvas:abc" / "audio:0.013"），
// 让不同维度的相同 hash 值（理论上罕见）不会互相抵消信号。
func SimHash(pieces []string) uint64 {
	if len(pieces) == 0 {
		return 0
	}
	var acc [64]int
	seen := false
	for _, p := range pieces {
		if p == "" {
			continue
		}
		seen = true
		h := fnvHash64(p)
		for i := 0; i < 64; i++ {
			if h&(uint64(1)<<i) != 0 {
				acc[i]++
			} else {
				acc[i]--
			}
		}
	}
	if !seen {
		return 0
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if acc[i] > 0 {
			out |= uint64(1) << i
		}
	}
	return out
}

// HammingDistance 两个 64-bit SimHash 的汉明距离 ∈ [0, 64]。
//
// XOR 后 popcount = bit 翻转数。POPCNT CPU 指令一条搞定，<1ns。
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// FuzzyMatch 模糊匹配：距离 <= threshold 视为同设备。
//
// threshold <= 0 时退化为"精确匹配"（仅 a == b 命中）；threshold >= 64 总命中。
// 任一参数为 0（未计算）→ 不匹配（防 0 ↔ 0 "假阳性同设备"）。
func FuzzyMatch(a, b uint64, threshold int) bool {
	if a == 0 || b == 0 {
		return false
	}
	if threshold < 0 {
		threshold = 0
	}
	return HammingDistance(a, b) <= threshold
}

// fnvHash64 用 64-bit FNV-1a 算 piece 字符串 hash。标准库 hash/fnv 即可。
// FNV 在 64-bit 下分布够均匀，速度比 sha256 快 10x，SimHash 不需要密码学强度。
func fnvHash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
