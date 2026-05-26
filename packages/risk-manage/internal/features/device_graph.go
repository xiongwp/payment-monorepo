package features

import (
	"context"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/fphash"
	"github.com/xiongwp/risk-manage/internal/store"
)

// DeviceGraphExtractor 设备维度反向聚合（与 CustomerHistoryExtractor 对称）。
//
// 输出字段：
//   DeviceDistinctCustomers90d ← LinkStore.Peers(device:X, "customer:")
//   DeviceFingerprintSimHash   ← fphash.SimHash(各信号 piece 拼接)
//
// 为什么独立成一个 extractor 而不并到 CustomerHistoryExtractor：
//   - 关注维度不同：CustomerHistory 看"客户用了多少 device / IP"，本 extractor
//     看"device 被多少 customer 用过"。fraud ring 经常是后者命中（一台机器跑
//     很多账号），前者命中典型是"一个用户多设备"也合法（家庭场景）。
//   - 失败语义独立：device fingerprint 缺失时此 extractor no-op，不影响
//     CustomerHistory 的 IsNewDevice 计算。
//
// 时间窗：LinkStore Mem 实现 TTL = 1h，所以"90d"是希望中的语义；
// 接 Redis backed store 后 TTL 直接配 90×24h 即达成。这里跟 CustomerHistory
// 同步 — 名字保留 *90d，行为靠 LinkStore TTL 控。
type DeviceGraphExtractor struct {
	links store.LinkStore
}

func NewDeviceGraphExtractor(links store.LinkStore) *DeviceGraphExtractor {
	return &DeviceGraphExtractor{links: links}
}

func (e *DeviceGraphExtractor) Name() string { return "device_graph" }

func (e *DeviceGraphExtractor) Enrich(ctx context.Context, txn *engine.TxnContext) {
	if txn == nil {
		return
	}

	// 1. Device → customer 反向去重计数
	if e.links != nil && txn.DeviceID != "" && txn.DeviceDistinctCustomers90d == 0 {
		devKey := "device:" + txn.DeviceID
		customers := e.links.Peers(ctx, devKey, "customer:")
		txn.DeviceDistinctCustomers90d = len(customers)
	}

	// 2. SimHash 计算（即便没 DeviceID 也能算 — 给后续 fuzzy link 用）
	if txn.DeviceFingerprintSimHash == 0 {
		pieces := devicePieces(txn)
		if len(pieces) > 0 {
			txn.DeviceFingerprintSimHash = fphash.SimHash(pieces)
		}
	}
}

// devicePieces 把 TxnContext 上的各设备信号拼成 SimHash piece slice。
// piece 形如 "<dim>:<value>"；空 value 跳过（避免 "canvas:" 这样无意义字符串
// 让多个未上报设备 SimHash 趋同）。
//
// 选用的信号是设备稳定性"中段"：
//   - 精确 sha256 已经包含 canvas / webgl / audio / font hash
//   - SimHash 同时包含这些 + UA / platform / screen / timezone / language
//     等"小升级会变 1-2 个"的信号，让浏览器升级不破坏识别
func devicePieces(txn *engine.TxnContext) []string {
	if txn == nil {
		return nil
	}
	out := make([]string, 0, 16)
	add := func(dim, val string) {
		if val != "" {
			out = append(out, dim+":"+val)
		}
	}
	add("canvas", txn.CanvasFingerprint)
	add("webgl", txn.WebGLRenderer)
	add("audio", txn.AudioContextHash)
	add("font", txn.FontHash)
	add("plugins", txn.PluginsHash)
	add("codec", txn.CodecHash)
	add("screen", txn.ScreenWxH)
	add("tz", txn.Timezone)
	add("lang", txn.Language)
	add("platform", txn.Platform)
	add("ua", txn.UserAgent)
	if txn.HardwareConcurrency > 0 {
		add("hc", itoaSmall(txn.HardwareConcurrency))
	}
	if txn.ColorDepth > 0 {
		add("cd", itoaSmall(txn.ColorDepth))
	}
	if txn.DeviceMemory > 0 {
		// DeviceMemory 是 float64（navigator.deviceMemory 一般 0.5/1/2/4/8 整数 GB）
		// 拼成定 string 让 SimHash 稳定
		add("dm", floatToFixed1(txn.DeviceMemory))
	}
	return out
}

// itoaSmall 不引 strconv（避免额外 import）；本 extractor 用的值都 < 1000
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// floatToFixed1 把 float64 编成一位小数定 string（"4" / "4.5"）。
// 跟 strconv.FormatFloat(v, 'f', 1, 64) 行为一致但不引 strconv 包。
func floatToFixed1(v float64) string {
	// 仅处理常见 0..32 GB 区间；超出范围用 fallback "x"（不进入 SimHash）
	if v < 0 || v > 1000 {
		return "x"
	}
	whole := int(v)
	frac := int((v-float64(whole))*10 + 0.5)
	if frac >= 10 {
		whole++
		frac = 0
	}
	if frac == 0 {
		return itoaSmall(whole)
	}
	return itoaSmall(whole) + "." + itoaSmall(frac)
}
