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
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"
)

// CardServiceClient 抽象 user-merchant-core 暴露的 user_card 子服务。
// 由 main.go 注入具体 mTLS gRPC 实现（user-merchant-core 加了 UserCardService 后接通）。
type CardServiceClient interface {
	AddCard(ctx context.Context, in *AddCardReq) (*AddCardResp, error)
	ListCards(ctx context.Context, userID int64) ([]CardInfo, error)
	DeleteCard(ctx context.Context, userID, userCardID int64) error
	SetDefaultCard(ctx context.Context, userID, userCardID int64) error
}

// AddCardReq PCI 注意：PAN/CVV 仅在本结构体内、调用栈中存在
type AddCardReq struct {
	UserID     int64
	PAN        string
	ExpMonth   int
	ExpYear    int
	CVV        string
	HolderName string
	SetDefault bool
	TraceID    string
}

type AddCardResp struct {
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
}

// NewCardHandler 构造
func NewCardHandler(base *Handler, cards CardServiceClient, pay PaymentServiceClient) *CardHandler {
	return &CardHandler{Handler: base, Cards: cards, Payments: pay}
}

// Register 把卡支付路由挂到 mux。
func (h *CardHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/cards", h.handleCardsRoute)       // GET 列表 / POST 绑卡
	mux.HandleFunc("/cards/new", h.handleCardNew)      // 表单
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

// /cards GET 列表 / POST 绑卡
func (h *CardHandler) handleCardsRoute(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.listCards(w, r, uid)
	case http.MethodPost:
		h.submitCard(w, r, uid)
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

func (h *CardHandler) submitCard(w http.ResponseWriter, r *http.Request, uid int64) {
	_ = r.ParseForm()
	pan := strings.ReplaceAll(r.FormValue("pan"), " ", "")
	expMonth, _ := strconv.Atoi(r.FormValue("exp_month"))
	expYear, _ := strconv.Atoi(r.FormValue("exp_year"))
	cvv := r.FormValue("cvv")
	holder := strings.TrimSpace(r.FormValue("holder_name"))
	setDefault := r.FormValue("set_default") == "1"
	defer func() {
		// PCI 纪律：本函数返回前清空 PAN/CVV 局部变量
		pan = ""
		cvv = ""
	}()
	if len(pan) < 12 || expMonth < 1 || expMonth > 12 || expYear < 2024 {
		h.render(w, "cards_new.html", map[string]any{
			"Title": "绑卡", "Flash": "卡号 / 有效期不合法",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.Cards.AddCard(ctx, &AddCardReq{
		UserID: uid, PAN: pan, ExpMonth: expMonth, ExpYear: expYear,
		CVV: cvv, HolderName: holder, SetDefault: setDefault,
	})
	if err != nil {
		h.render(w, "cards_new.html", map[string]any{
			"Title": "绑卡", "Flash": grpcMsg(err),
		})
		return
	}
	h.logger.Info("card added",
		zap.String("masked_pan", resp.MaskedPAN),
		zap.String("network", resp.Network),
		zap.Int64("user_card_id", resp.UserCardID))
	http.Redirect(w, r, "/cards", http.StatusFound)
}

func (h *CardHandler) handleCardNew(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.render(w, "cards_new.html", map[string]any{"Title": "绑卡"})
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
		resp, err := h.Payments.CreateAndConfirmCardPayment(ctx, &CreatePIReq{
			UserID: uid, UserCardID: userCardID,
			Amount: amount, Currency: currency,
			Description: desc, IdempotencyKey: idem,
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
