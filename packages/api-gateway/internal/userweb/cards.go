// cards.go: 用户绑卡 / 列卡 / 删卡 / 选卡支付 HTTP handlers。
//
// 路由：
//
//	GET  /cards            列出当前用户存卡（脱敏后）
//	POST /cards            绑卡：PAN 透传到 user-merchant-core → card-center.Tokenize
//	GET  /cards/new        绑卡表单
//	POST /cards/{id}/delete   删卡
//	POST /cards/{id}/default  设默认
//	GET  /pay              支付页：金额输入 + 卡选择
//	POST /pay              提交支付：order-core.CreateAndConfirmCardPayment
//	GET  /pay/result?pi=    支付结果
//
// PCI 边界纪律：
//   - api-gateway 进程 PAN 仅在 POST /cards handler 的 r.Form 字段里
//   - 立即调 user-merchant-core.AddCard（mTLS gRPC，PAN 经过 wire 一次）
//   - 函数返回前 defer 清栈
//   - 真实生产：前端 SDK 直接 HTTPS POST 到 card-center，绕过 api-gateway，
//     这样 api-gateway / user-merchant-core 永远不见 PAN，scope 收窄到 SAQ-A
package userweb

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1"
)

// CardServiceClient 抽象 user-merchant-core 暴露的 user_card 子服务。
// 由 main.go 注入具体 mTLS gRPC 实现（user-merchant-core 加了 UserCardService 后接通）。
type CardServiceClient interface {
	// AttachCard 把已经在 card-center 拿到的 stored_token + 元数据持久化进 user_card 表。
	// **不接受 PAN** —— PAN 在浏览器 → card-center 之间就已经换成了 stored_token。
	AttachCard(ctx context.Context, in *AttachCardReq) (*AttachCardResp, error)
	ListCards(ctx context.Context, userID int64) ([]CardInfo, error)
	DeleteCard(ctx context.Context, userID, userCardID int64) error
	SetDefaultCard(ctx context.Context, userID, userCardID int64) error
}

// AttachCardReq PCI 严格纪律：本结构体内**不存在 PAN/CVV 字段**。
// PAN 仅活在 浏览器 ↔ card-center 之间的 HTTPS 单跳里；本服务的所有路径都看不见 PAN。
type AttachCardReq struct {
	UserID      int64
	StoredToken string // card-center 返的长期 token（KMS-encrypted blob）
	MaskedPAN   string // BIN+last4，展示用
	Network     string // visa / mastercard / ...
	ExpMonth    int
	ExpYear     int
	HolderName  string
	SetDefault  bool
	TraceID     string
}

type AttachCardResp struct {
	UserCardID int64
	MaskedPAN  string
	Network    string
}

type CardInfo struct {
	ID         int64
	MaskedPAN  string
	Network    string
	ExpMonth   int
	ExpYear    int
	HolderName string
	IsDefault  bool
}

// PaymentServiceClient 抽象 order-core 的卡支付入口。
type PaymentServiceClient interface {
	CreateAndConfirmCardPayment(ctx context.Context, in *CreatePIReq) (*CreatePIResp, error)
	GetPI(ctx context.Context, piID string) (*PIInfo, error)
}

type CreatePIReq struct {
	UserID         int64
	UserCardID     int64
	Amount         int64
	Currency       string
	Description    string
	IdempotencyKey string
	// MchID 商户标识；卡支付链路里用户是付款方，但 order-core 强制 mch_id 非空，
	// 留空 → grpcPaymentClient 用 dev-merchant-001 兜底
	MchID          string
}

type CreatePIResp struct {
	PIID          string
	Status        string
	DeclineCode   string
	DeclineReason string
}

type PIInfo struct {
	ID            string
	Amount        int64
	Currency      string
	Status        string
	MaskedPAN     string
	Network       string
	DeclineReason string
}

// CardHandler 卡支付 HTTP handler；embed Handler 复用 render / cookie / 鉴权
type CardHandler struct {
	*Handler
	Cards    CardServiceClient
	Payments PaymentServiceClient
	// CardCenterURL 浏览器端 SDK / form JS 直连 card-center 用的公网 URL，
	// 注入到 cards_new.html 模板。空 = 前端会显示"未配置"错误。
	CardCenterURL string
}

// NewCardHandler 构造
func NewCardHandler(base *Handler, cards CardServiceClient, pay PaymentServiceClient) *CardHandler {
	return &CardHandler{Handler: base, Cards: cards, Payments: pay}
}

// Register 把卡支付路由挂到 mux。
//
// 路由：
//
//	GET  /cards         列表
//	POST /cards/attach  绑卡持久化（**只**接收 stored_token，绝不接受 PAN）
//	GET  /cards/new     绑卡 form（JS 直发 card-center HTTPS）
//	POST /cards/{id}/{delete,default}
//	GET/POST /pay  /pay/result
func (h *CardHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/cards", h.handleCardsRoute)       // GET 列表（POST 已废弃，移到 /cards/attach）
	mux.HandleFunc("/cards/attach", h.handleAttach)    // POST stored_token 持久化
	mux.HandleFunc("/cards/new", h.handleCardNew)      // 表单（JS 直发 card-center）
	mux.HandleFunc("/cards/", h.handleCardOps)         // /cards/:id/{delete,default}
	mux.HandleFunc("/pay", h.handlePay)                // GET 支付页 / POST 提交
	mux.HandleFunc("/pay/result", h.handlePayResult)
}

// requireUser 鉴权 + 拿 user_id；未登录跳 /login
func (h *CardHandler) requireUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	jwt, ok := readJWTCookie(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return 0, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	intro, err := h.uc.IntrospectToken(ctx, &usermerchantv1.IntrospectTokenRequest{Jwt: jwt})
	if err != nil || !intro.GetValid() {
		h.clearCookie(w, CookieName)
		http.Redirect(w, r, "/login", http.StatusFound)
		return 0, false
	}
	uid, _ := strconv.ParseInt(intro.GetUserId(), 10, 64)
	return uid, true
}

// /cards GET 列表（POST 已废弃，PAN 不再走 api-gateway；用 /cards/attach 接收 stored_token）
func (h *CardHandler) handleCardsRoute(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.listCards(w, r, uid)
	case http.MethodPost:
		// 早期 form POST 路径已废弃 —— PAN 不再经过 api-gateway。
		// 浏览器应该改成 fetch card-center HTTPS 拿 stored_token，再 POST /cards/attach。
		http.Error(w, "POST /cards 已废弃；改用 fetch card-center HTTPS + POST /cards/attach", http.StatusGone)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *CardHandler) listCards(w http.ResponseWriter, r *http.Request, uid int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	cards, err := h.Cards.ListCards(ctx, uid)
	if err != nil {
		h.logger.Warn("list cards", zap.Error(err))
		cards = nil
	}
	h.render(w, "cards.html", map[string]any{"Title": "我的卡", "Cards": cards})
}

// handleAttach 浏览器拿到 card-center 的 stored_token 后回 POST 这里持久化。
//
// PCI 严格：**整个函数体内不允许引用 PAN / CVV 字段**。Go 静态检查 + code review 保证。
// body 协议（JSON）：
//
//	{
//	  "stored_token": "tok_card_xxx",
//	  "masked_pan":   "411111******1111",
//	  "network":      "visa",
//	  "exp_month":    12,
//	  "exp_year":     2030,
//	  "holder_name":  "ZHANG SAN",
//	  "set_default":  true
//	}
func (h *CardHandler) handleAttach(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		StoredToken string `json:"stored_token"`
		MaskedPAN   string `json:"masked_pan"`
		Network     string `json:"network"`
		ExpMonth    int    `json:"exp_month"`
		ExpYear     int    `json:"exp_year"`
		HolderName  string `json:"holder_name"`
		SetDefault  bool   `json:"set_default"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		http.Error(w, "bad json body", http.StatusBadRequest)
		return
	}
	// 校验：stored_token 必须是 card-center 颁发的格式 tok_card_*；防止前端误传 PAN
	if !strings.HasPrefix(in.StoredToken, "tok_card_") {
		http.Error(w, "stored_token format invalid (must start with tok_card_)", http.StatusBadRequest)
		return
	}
	if in.MaskedPAN == "" || in.Network == "" {
		http.Error(w, "masked_pan / network required", http.StatusBadRequest)
		return
	}
	if in.ExpMonth < 1 || in.ExpMonth > 12 || in.ExpYear < 2024 {
		http.Error(w, "exp invalid", http.StatusBadRequest)
		return
	}
	// 防止 masked_pan 字段实际写了完整 PAN（异常前端 / 中间人攻击）
	// 真实 masked 不会超过 20 字符且必须包含 *
	if len(in.MaskedPAN) > 20 || !strings.Contains(in.MaskedPAN, "*") {
		http.Error(w, "masked_pan looks unmasked; rejected", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.Cards.AttachCard(ctx, &AttachCardReq{
		UserID:      uid,
		StoredToken: in.StoredToken,
		MaskedPAN:   in.MaskedPAN,
		Network:     in.Network,
		ExpMonth:    in.ExpMonth,
		ExpYear:     in.ExpYear,
		HolderName:  in.HolderName,
		SetDefault:  in.SetDefault,
	})
	if err != nil {
		h.logger.Warn("attach card", zap.Error(err))
		http.Error(w, "attach failed: "+grpcMsg(err), http.StatusBadGateway)
		return
	}
	h.logger.Info("card attached",
		zap.String("masked_pan", resp.MaskedPAN),
		zap.String("network", resp.Network),
		zap.Int64("user_card_id", resp.UserCardID))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"user_card_id": resp.UserCardID,
		"masked_pan":   resp.MaskedPAN,
		"network":      resp.Network,
	})
}

func (h *CardHandler) handleCardNew(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.render(w, "cards_new.html", map[string]any{
		"Title":         "绑卡",
		"CardCenterURL": h.CardCenterURL, // 浏览器 JS 直发的目标域
	})
}

func (h *CardHandler) handleCardOps(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/cards/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	cardID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad card id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	switch parts[1] {
	case "delete":
		if err := h.Cards.DeleteCard(ctx, uid, cardID); err != nil {
			http.Error(w, "delete failed: "+grpcMsg(err), http.StatusInternalServerError)
			return
		}
	case "default":
		if err := h.Cards.SetDefaultCard(ctx, uid, cardID); err != nil {
			http.Error(w, "set default failed: "+grpcMsg(err), http.StatusInternalServerError)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/cards", http.StatusFound)
}

// /pay 支付
func (h *CardHandler) handlePay(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		cards, _ := h.Cards.ListCards(ctx, uid)
		h.render(w, "pay.html", map[string]any{"Title": "支付", "Cards": cards})
	case http.MethodPost:
		_ = r.ParseForm()
		amount, _ := strconv.ParseInt(r.FormValue("amount"), 10, 64)
		currency := strings.TrimSpace(r.FormValue("currency"))
		userCardID, _ := strconv.ParseInt(r.FormValue("user_card_id"), 10, 64)
		desc := strings.TrimSpace(r.FormValue("description"))
		if currency == "" {
			currency = "PHP"
		}
		if amount <= 0 || userCardID == 0 {
			h.render(w, "pay.html", map[string]any{"Title": "支付", "Flash": "金额 / 卡 必填"})
			return
		}
		// idempotency_key = user + card + amount + 时间窗（5min 内重复提交视为同笔）
		idem := strings.Join([]string{
			strconv.FormatInt(uid, 10),
			strconv.FormatInt(userCardID, 10),
			strconv.FormatInt(amount, 10),
			currency,
			strconv.FormatInt(time.Now().Unix()/300, 10),
		}, ":")
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		mchID := strings.TrimSpace(r.FormValue("mch_id"))
		if mchID == "" {
			h.render(w, "pay.html", map[string]any{"Title": "支付", "Cards": h.listCardsSafe(r, uid), "Flash": "商户 ID 必填"})
			return
		}
		resp, err := h.Payments.CreateAndConfirmCardPayment(ctx, &CreatePIReq{
			UserID: uid, UserCardID: userCardID,
			Amount: amount, Currency: currency,
			Description: desc, IdempotencyKey: idem, MchID: mchID,
		})
		if err != nil {
			h.render(w, "pay.html", map[string]any{"Title": "支付", "Flash": "支付失败：" + grpcMsg(err)})
			return
		}
		http.Redirect(w, r, "/pay/result?pi="+resp.PIID, http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *CardHandler) handlePayResult(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	piID := r.URL.Query().Get("pi")
	if piID == "" {
		http.Redirect(w, r, "/pay", http.StatusFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	pi, err := h.Payments.GetPI(ctx, piID)
	if err != nil {
		h.render(w, "pay_result.html", map[string]any{"Title": "支付结果", "Flash": grpcMsg(err)})
		return
	}
	h.render(w, "pay_result.html", map[string]any{"Title": "支付结果", "PI": pi})
}

// listCardsSafe 给 handlePay 出错重渲染时复用，吞掉 ListCards 错误（已经在出错路径，
// 不要再上报第二次错），返回值可能 nil。
func (h *CardHandler) listCardsSafe(r *http.Request, uid int64) []CardInfo {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	cards, _ := h.Cards.ListCards(ctx, uid)
	return cards
}
