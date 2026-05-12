// Package httpapi — Approval HTTP API + SSE 实时流.
//
// 路由:
//   POST   /v1/actions                       创建 (业务侧调)
//   GET    /v1/actions?state=&type=&limit=   列
//   GET    /v1/actions/{id}                  详情
//   POST   /v1/actions/{id}/approve          复核通过
//   POST   /v1/actions/{id}/reject           复核拒绝
//   POST   /v1/actions/{id}/cancel           requester 撤回
//   POST   /v1/actions/{id}/execute          业务侧执行后回写
//   GET    /v1/stream                        SSE 实时事件流 (biz-admin-web 接)
//
// 这个服务用 payment-util/approval 当后端 (Coordinator + MemStore);
// 真生产换 MySQL Store + audit-log HTTPSink.

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// 跟 payment-util/approval 接口对齐的本地副本 — 避免跨模块 import 复杂度.
// 真生产用 go-work + import "github.com/.../payment-util/approval".

type State string
type Decision string

const (
	StatePending   State = "pending"
	StateReviewing State = "reviewing"
	StateApproved  State = "approved"
	StateRejected  State = "rejected"
	StateExecuted  State = "executed"
	StateExpired   State = "expired"
	StateCancelled State = "cancelled"

	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
)

type Action struct {
	ID                string                 `json:"id"`
	Type              string                 `json:"type"`
	Resource          string                 `json:"resource"`
	Requester         string                 `json:"requester"`
	RequesterAt       time.Time              `json:"requester_at"`
	RequestNote       string                 `json:"request_note,omitempty"`
	RequiredApprovals int                    `json:"required_approvals"`
	State             State                  `json:"state"`
	Approvals         []ApprovalRecord       `json:"approvals"`
	Payload           map[string]interface{} `json:"payload,omitempty"`
	ExpiresAt         time.Time              `json:"expires_at"`
}

type ApprovalRecord struct {
	Reviewer   string    `json:"reviewer"`
	Decision   Decision  `json:"decision"`
	Note       string    `json:"note,omitempty"`
	ReviewedAt time.Time `json:"reviewed_at"`
}

// Server in-memory; 真生产换 SQL store.
type Server struct {
	mu      sync.RWMutex
	actions map[string]*Action
	subs    map[chan string]struct{}
	log     *zap.Logger
}

func New(log *zap.Logger) *Server {
	if log == nil {
		log = zap.NewNop()
	}
	return &Server{
		actions: make(map[string]*Action),
		subs:    make(map[chan string]struct{}),
		log:     log,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/actions", s.routeActions)
	mux.HandleFunc("/v1/actions/", s.routeActionByID)
	mux.HandleFunc("/v1/stream", s.handleSSE)
	return mux
}

// POST  /v1/actions      create
// GET   /v1/actions      list
func (s *Server) routeActions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.create(w, r)
	case http.MethodGet:
		s.list(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
	}
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var in Action
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if in.Type == "" || in.Resource == "" || in.Requester == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "type, resource, requester required")
		return
	}
	if in.ID == "" {
		in.ID = fmt.Sprintf("ap_%d_%s", time.Now().UnixNano(), in.Resource)
	}
	if in.RequiredApprovals == 0 {
		in.RequiredApprovals = 2
	}
	in.State = StatePending
	in.RequesterAt = time.Now().UTC()
	if in.ExpiresAt.IsZero() {
		in.ExpiresAt = in.RequesterAt.Add(7 * 24 * time.Hour)
	}
	s.mu.Lock()
	s.actions[in.ID] = &in
	s.mu.Unlock()
	s.publish("created:" + in.ID)
	writeJSON(w, http.StatusCreated, in)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	typ := q.Get("type")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit == 0 || limit > 500 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Action, 0, len(s.actions))
	for _, a := range s.actions {
		if state != "" && string(a.State) != state {
			continue
		}
		if typ != "" && a.Type != typ {
			continue
		}
		out = append(out, *a)
	}
	// 按 requester_at desc 排
	sortActions(out)
	if len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"actions": out,
		"count":   len(out),
	})
}

// /v1/actions/{id}[/(approve|reject|cancel|execute)]
func (s *Server) routeActionByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/actions/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeErr(w, http.StatusBadRequest, "bad_path", "missing id")
		return
	}
	if len(parts) == 1 {
		if r.Method == http.MethodGet {
			s.get(w, id)
			return
		}
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	action := parts[1]
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	switch action {
	case "approve":
		s.decide(w, r, id, DecisionApprove)
	case "reject":
		s.decide(w, r, id, DecisionReject)
	case "cancel":
		s.cancel(w, r, id)
	case "execute":
		s.execute(w, r, id)
	default:
		writeErr(w, http.StatusBadRequest, "bad_action", action)
	}
}

func (s *Server) get(w http.ResponseWriter, id string) {
	s.mu.RLock()
	a, ok := s.actions[id]
	s.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", id)
		return
	}
	writeJSON(w, http.StatusOK, *a)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, id string, dec Decision) {
	var body struct {
		Reviewer string `json:"reviewer"`
		Note     string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Reviewer == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "reviewer required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.actions[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", id)
		return
	}
	if a.State != StatePending && a.State != StateReviewing {
		writeErr(w, http.StatusConflict, "already_decided", string(a.State))
		return
	}
	if body.Reviewer == a.Requester {
		writeErr(w, http.StatusForbidden, "self_approval", "")
		return
	}
	for _, p := range a.Approvals {
		if p.Reviewer == body.Reviewer {
			writeErr(w, http.StatusConflict, "duplicate_reviewer", "")
			return
		}
	}
	a.Approvals = append(a.Approvals, ApprovalRecord{
		Reviewer:   body.Reviewer,
		Decision:   dec,
		Note:       body.Note,
		ReviewedAt: time.Now().UTC(),
	})
	if dec == DecisionReject {
		a.State = StateRejected
	} else {
		approveCount := 0
		for _, p := range a.Approvals {
			if p.Decision == DecisionApprove {
				approveCount++
			}
		}
		if approveCount >= a.RequiredApprovals {
			a.State = StateApproved
		} else {
			a.State = StateReviewing
		}
	}
	s.publish("updated:" + id)
	writeJSON(w, http.StatusOK, *a)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Requester string `json:"requester"`
		Reason    string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.actions[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", id)
		return
	}
	if a.Requester != body.Requester {
		writeErr(w, http.StatusForbidden, "not_requester", "only requester can cancel")
		return
	}
	switch a.State {
	case StatePending, StateReviewing, StateApproved:
		a.State = StateCancelled
	default:
		writeErr(w, http.StatusConflict, "invalid_state", string(a.State))
		return
	}
	s.publish("updated:" + id)
	writeJSON(w, http.StatusOK, *a)
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.actions[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", id)
		return
	}
	if a.State != StateApproved {
		writeErr(w, http.StatusConflict, "not_approved", string(a.State))
		return
	}
	a.State = StateExecuted
	s.publish("updated:" + id)
	writeJSON(w, http.StatusOK, *a)
}

// SSE — biz-admin-web 订阅
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关 nginx 缓冲

	ch := make(chan string, 32)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
		close(ch)
	}()

	// hello + keepalive 每 25s
	fmt.Fprintf(w, "event: hello\ndata: {\"connected\":true}\n\n")
	flusher.Flush()
	tk := time.NewTicker(25 * time.Second)
	defer tk.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			fmt.Fprintf(w, "event: action_update\ndata: %q\n\n", ev)
			flusher.Flush()
		case <-tk.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) publish(msg string) {
	s.mu.RLock()
	subs := make([]chan string, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- msg:
		default:
			// 满了 drop, 不阻塞
		}
	}
}

// ── helpers ──

func sortActions(a []Action) {
	for i := 1; i < len(a); i++ {
		j := i
		for j > 0 && a[j].RequesterAt.After(a[j-1].RequesterAt) {
			a[j], a[j-1] = a[j-1], a[j]
			j--
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"error": errCode, "message": msg})
}
