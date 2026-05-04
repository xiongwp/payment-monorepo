package service

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// idemSep 用作字段分隔符。选 NUL 而非 ":" 是为了避免任何业务字段（pi_id /
// refund_id / currency 等）里若意外混入冒号/连字符时造成"键碰撞"——例如
// pi_id="a:b" + action="c" 与 pi_id="a" + action="b:c" 冒号分隔下会得到同一摘要。
// NUL 在合法的 ASCII / UTF-8 标识符里不会出现，碰撞概率为零。
const idemSep = "\x00"

// IdempotencyKey = sha256(pi_id ␀ action ␀ amount ␀ currency)
//
// 资损背景：原实现只 hash pi_id+action，**未纳入金额 / 币种**。一旦上游对同一
// pi 改金额（半价券生效后重发）或同 pi_id 不同币种（多币账户切换），
// payment-channel 侧 UNIQUE(idempotency_key) 会命中旧记录直接返回成功，
// 但**真实资金从未动过原金额**，造成"以为按新金额扣了，实际按旧金额"或
// 直接漏扣。把 amount + currency 进 hash 后任何金额/币种变化都会得到新 key，
// 强制下游重新落账。
//
// 由 payment-core 计算后透传给 payment-channel，确保同一笔支付 / 退款等
// 在 channel 侧 UNIQUE 命中，绝不重复下单。
func IdempotencyKey(piID, action string, amount int64, currency string) string {
	h := sha256.New()
	h.Write([]byte(piID))
	h.Write([]byte(idemSep))
	h.Write([]byte(action))
	h.Write([]byte(idemSep))
	h.Write([]byte(strconv.FormatInt(amount, 10)))
	h.Write([]byte(idemSep))
	h.Write([]byte(currency))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)
}

// RefundIdempotencyKey = sha256(pi_id ␀ refund_id ␀ amount ␀ currency ␀ "refund")
//
// 退款独立 key：同一笔 charge 上可能产生多次部分退款，
// 必须以 (pi_id, refund_id, amount, currency) 元组唯一。
// refund_id 由 order-core 生成（一退一 id），amount/currency 进 hash 后
// 任何金额/币种变化都会强制新 key。
func RefundIdempotencyKey(piID, refundID string, amount int64, currency string) string {
	h := sha256.New()
	h.Write([]byte(piID))
	h.Write([]byte(idemSep))
	h.Write([]byte(refundID))
	h.Write([]byte(idemSep))
	h.Write([]byte(strconv.FormatInt(amount, 10)))
	h.Write([]byte(idemSep))
	h.Write([]byte(currency))
	h.Write([]byte(idemSep))
	h.Write([]byte("refund"))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)
}
