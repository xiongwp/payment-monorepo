// handler.go: card-center HTTPS REST endpoints。
//
// 暴露给前端 SDK / 浏览器的 HTTPS API（不是 mTLS gRPC）。所有请求都经
// Middleware 校验 jwt → ctx user_id；handler 用 ctx user_id 操作，**绝不**
// 信任 body / query / form 里的 user_id 参数。
//
// 路由：
//
//	POST /v1/cards         绑卡（PAN/CVV → stored_token）
//	GET  /v1/cards         列卡（masked-only）
//	DELETE /v1/cards/{id}  删卡（soft delete）
//
// 注意：CreatePaymentToken / Detokenize 不在 HTTPS 暴露 —— 那是 mTLS gRPC
// 内部接口，调用方是 order-core / card-payment。
package httpsauth

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/xiongwp/card-center/internal/service"
)

// CardOps 抽 RESTServer 用到的 service 方法，方便单测注假实现。
//
// *service.Service 自动满足本接口；NewRESTServer 接口而不是具体类型让 handler
// 可以单测越权 / 错误传播 / 输入校验，不依赖 vault / repo / KMS 一整套真实栈。
type CardOps interface {
	Tokenize(ctx context.Context, in *service.TokenizeInput) (*service.TokenizeOutput, error)
	ListUserCards(ctx context.Context, userID int64, caller, callerIP, traceID string) ([]*service.CardDisplay, error)
	DeleteCardByID(ctx context.Context, userID, userCardID int64, reason, caller, callerIP, traceID string) (*service.CardDisplay, error)
}

// RESTServer 暴露 HTTPS REST endpoints。
type RESTServer struct {
	svc      CardOps
	verifier Verifier
	limiter  RateLimiter // 仅 tokenize 路径用；nil = 不限流
	logger   *zap.Logger
}

// NewRESTServer 构造
func NewRESTServer(svc CardOps, verifier Verifier, logger *zap.Logger) *RESTServer {
	return &RESTServer{svc: svc, verifier: verifier, logger: logger}
}

// WithRateLimiter 注入 tokenize 限流器。生产推荐配；dev 不配。
func (s *RESTServer) WithRateLimiter(rl RateLimiter) *RESTServer {
	s.limiter = rl
	return s
}

// Handler 返回总 mux：/healthz 直通，/v1/* 套 auth middleware + 安全头。
//
// 路由细节：
//   - GET  /v1/cards   → auth → listCards
//   - POST /v1/cards   → auth → 限流 → tokenizeCard （限流仅作用于 POST）
//   - DELETE /v1/cards/{id} → auth → deleteCard
func (s *RESTServer) Handler() http.Handler {
	authed := Middleware(s.verifier, s.logger)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// /v1/cards：method-aware 装配，POST 单独套限流
	cardsRoot := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.listCards(w, r)
		case http.MethodPost:
			// limiter 包一层；nil 时直通
			h := http.Handler(http.HandlerFunc(s.tokenizeCard))
			if s.limiter != nil {
				h = RateLimitMiddleware(s.limiter, s.logger)(h)
			}
			h.ServeHTTP(w, r)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		}
	})
	mux.Handle("/v1/cards", authed(cardsRoot))
	mux.Handle("/v1/cards/", authed(http.HandlerFunc(s.dispatchCardOps)))
	return securityHeaders(mux)
}

// securityHeaders 给所有响应注入 HSTS / X-Frame-Options / 其它常规安全头。
//
//   - HSTS：防降级到 HTTP；includeSubDomains 因为 card-center 域是隔离的，安全
//   - Cache-Control：卡列表绝对不能被任何中间缓存层留住
//   - X-Content-Type-Options：防嗅探
//   - Referrer-Policy：no-referrer 避免外链泄露 jwt
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// luhnValid Luhn 校验
func luhnValid(pan string) bool {
	// 滤空格
	digits := make([]byte, 0, len(pan))
	for i := 0; i < len(pan); i++ {
		c := pan[i]
		if c == ' ' || c == '-' {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
		digits = append(digits, c-'0')
	}
	if len(digits) < 12 || len(digits) > 19 {
		return false
	}
	sum := 0
	dbl := false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i])
		if dbl {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		dbl = !dbl
	}
	return sum%10 == 0
}

// /v1/cards/{id} DELETE 删卡
func (s *RESTServer) dispatchCardOps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/v1/cards/")
	if idStr == "" || strings.Contains(idStr, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad path"})
		return
	}
	cardID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad card id"})
		return
	}
	s.deleteCard(w, r, cardID)
}

// ─── handlers ──────────────────────────────────────────────────────────────

// listCards 仅返脱敏字段（service.ListUserCards 已做白名单投影）。
func (s *RESTServer) listCards(w http.ResponseWriter, r *http.Request) {
	uid, ok := UserIDFromCtx(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}
	cards, err := s.svc.ListUserCards(r.Context(), uid, "card-center-https", clientIP(r), JWTHashFromCtx(r.Context()))
	if err != nil {
		s.logger.Warn("listCards failed", zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list cards failed"})
		return
	}
	// 返结果用 JSON 友好的 view
	type cardView struct {
		ID         int64  `json:"id"`
		MaskedPAN  string `json:"masked_pan"` // 展示 only
		Network    string `json:"network"`
		ExpMonth   int    `json:"exp_month"`
		ExpYear    int    `json:"exp_year"`
		HolderName string `json:"holder_name,omitempty"`
		Status     string `json:"status"`
	}
	out := make([]cardView, 0, len(cards))
	for _, c := range cards {
		out = append(out, cardView{
			ID: c.UserCardID, MaskedPAN: c.MaskedPAN, Network: c.Network,
			ExpMonth: c.ExpMonth, ExpYear: c.ExpYear,
			HolderName: c.HolderName, Status: c.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": out})
}

// tokenizeCard 绑卡。PAN/CVV 在本函数 stack 内出现一次。
//
// 接收 user_id：**只**从 ctx 取，body 里就算传了 user_id 也忽略 + 拒绝。
func (s *RESTServer) tokenizeCard(w http.ResponseWriter, r *http.Request) {
	uid, ok := UserIDFromCtx(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}
	var in struct {
		PAN        string `json:"pan"`
		ExpMonth   int    `json:"exp_month"`
		ExpYear    int    `json:"exp_year"`
		CVV        string `json:"cvv"` // 仅当前栈使用，不入库
		HolderName string `json:"holder_name"`
		// **故意不读** user_id：哪怕客户端传了也忽略。
		ClaimedUserID int64 `json:"user_id,omitempty"`
	}
	defer func() {
		// PCI 纪律：返回前清栈
		in.PAN = ""
		in.CVV = ""
		in.HolderName = ""
	}()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad body"})
		return
	}
	// 防越权（严格模式）：客户端**根本不允许**发 user_id 字段。
	// 即便发的跟 ctx 一致，也是不规范客户端，403 让前端修协议。
	// 这条 audit 进 log，方便发现误用 / 攻击行为。
	if err := RejectClaimedUserID(in.ClaimedUserID); err != nil {
		s.logger.Warn("rejected: client supplied user_id",
			zap.Int64("ctx_uid", uid),
			zap.Int64("claimed_uid", in.ClaimedUserID),
			zap.String("client_ip", clientIP(r)))
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "cross-user denied: do not include user_id in request body",
		})
		return
	}
	if in.PAN == "" || in.ExpMonth < 1 || in.ExpMonth > 12 || in.ExpYear < 2024 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pan / exp invalid"})
		return
	}
	// Luhn 校验：阻挡明显错误的卡号，避免给 KMS / DB 增压。
	// 真正的 BIN 范围 / network detection 在 vault 层做。
	if !luhnValid(in.PAN) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pan checksum invalid"})
		return
	}
	out, err := s.svc.Tokenize(r.Context(), &service.TokenizeInput{
		UserID:     uid, // ctx 取，权威
		PAN:        in.PAN,
		ExpMonth:   in.ExpMonth,
		ExpYear:    in.ExpYear,
		HolderName: in.HolderName,
		Caller:     "card-center-https",
		CallerIP:   clientIP(r),
		TraceID:    JWTHashFromCtx(r.Context()),
	})
	if err != nil {
		s.logger.Warn("tokenize failed", zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "tokenize failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stored_token": out.StoredToken, // 给浏览器 → 浏览器存到 api-gateway 的 user_card 表
		"masked_pan":   out.MaskedPAN,
		"network":      out.Network,
	})
}

// deleteCard 删卡。**仅删本人的卡**：service.DeleteCardByID 在 SQL 层就把
// user_id + id 一起匹配，攻击者拿到别人的 user_card_id 也删不掉别人的卡。
func (s *RESTServer) deleteCard(w http.ResponseWriter, r *http.Request, cardID int64) {
	uid, ok := UserIDFromCtx(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}
	out, err := s.svc.DeleteCardByID(r.Context(), uid, cardID,
		"user requested via HTTPS",
		"card-center-https", clientIP(r), JWTHashFromCtx(r.Context()))
	if err != nil {
		// 不暴露内部错误细节（既能命中 not-found 也能命中 dup-state）
		s.logger.Debug("deleteCard failed", zap.Int64("uid", uid), zap.Int64("card_id", cardID), zap.Error(err))
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "card not found or already deleted"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         out.UserCardID,
		"masked_pan": out.MaskedPAN,
		"status":     out.Status,
	})
}

// ─── helpers ───────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host != "" {
		return host
	}
	return r.RemoteAddr
}
