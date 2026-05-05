package channel

import "testing"

// VerifierContractCase 单条契约测试 case。
type VerifierContractCase struct {
	// Name 测试名（出现在 t.Run 子用例标题里）。
	Name string
	// Headers / Body 喂给 Verifier 的原始入参。
	Headers map[string]string
	Body    []byte
	// WantOK 期望 VerifyHeaders 返回的 ok。
	WantOK bool
}

// RunVerifierContract 跑标准 4 case：合法 / body 篡改 / sig 篡改 / 时间漂移。
//
// 每个 channel adapter 应该在自己的 *_test.go 写一个 makeVerifier 工厂函数，
// 然后调用本函数：
//
//	func TestWebhookVerifier_Contract(t *testing.T) {
//	    secret := "test_secret"
//	    body := []byte(`{"event":"charge.succeeded","amount":1000}`)
//	    validSig := channel.HMACSHA256Hex([]byte(secret), body)
//	    cases := []channel.VerifierContractCase{
//	        {Name: "valid", Headers: map[string]string{"X-Signature": validSig}, Body: body, WantOK: true},
//	        {Name: "tampered_body", Headers: map[string]string{"X-Signature": validSig}, Body: append(body, []byte("X")...), WantOK: false},
//	        {Name: "tampered_sig", Headers: map[string]string{"X-Signature": "00" + validSig[2:]}, Body: body, WantOK: false},
//	        {Name: "missing_sig", Headers: map[string]string{}, Body: body, WantOK: false},
//	    }
//	    channel.RunVerifierContract(t, NewMyAdapter(secret), cases)
//	}
//
// 这样跨 15 个 adapter 用同一组安全断言验收，避免某个 adapter 漏掉某 case。
func RunVerifierContract(t *testing.T, v WebhookVerifier, cases []VerifierContractCase) {
	t.Helper()
	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			ok, reason := v.VerifyHeaders(c.Headers, c.Body)
			if ok != c.WantOK {
				t.Fatalf("case %q: ok=%v reason=%q; want ok=%v",
					c.Name, ok, reason, c.WantOK)
			}
		})
	}
}
