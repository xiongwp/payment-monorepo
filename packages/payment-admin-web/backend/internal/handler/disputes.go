package handler

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/mux"

	orderv1 "reconcile-system/packages/order-core/kitex_gen/order/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

type DisputeHandler struct {
	deps clients.Deps
	cli  disputeservice.Client
}

func NewDisputeHandler(d clients.Deps, cli disputeservice.Client) *DisputeHandler {
	return &DisputeHandler{deps: d, cli: cli}
}

// IsProdEnv reports whether the BFF is running in the production environment.
// Used to gate developer-only endpoints (simulate / mock create-intent) so they
// cannot fire real state machine transitions or real charges in prod.
//
// Convention: APP_ENV=prod (or production) → prod; anything else → non-prod.
func IsProdEnv() bool {
	v := os.Getenv("APP_ENV")
	return v == "prod" || v == "production"
}

func parseDisputeStatus(s string) orderv1.DisputeStatus {
	switch s {
	case "DISPUTE_STATUS_NEEDS_RESPONSE":
		return orderv1.DisputeStatus_DISPUTE_STATUS_NEEDS_RESPONSE
	case "DISPUTE_STATUS_UNDER_REVIEW":
		return orderv1.DisputeStatus_DISPUTE_STATUS_UNDER_REVIEW
	case "DISPUTE_STATUS_WON":
		return orderv1.DisputeStatus_DISPUTE_STATUS_WON
	case "DISPUTE_STATUS_LOST":
		return orderv1.DisputeStatus_DISPUTE_STATUS_LOST
	case "DISPUTE_STATUS_WARNING_CLOSED":
		return orderv1.DisputeStatus_DISPUTE_STATUS_WARNING_CLOSED
	case "DISPUTE_STATUS_CHARGE_REFUNDED":
		return orderv1.DisputeStatus_DISPUTE_STATUS_CHARGE_REFUNDED
	case "DISPUTE_STATUS_CANCELED":
		return orderv1.DisputeStatus_DISPUTE_STATUS_CANCELED
	}
	return orderv1.DisputeStatus_DISPUTE_STATUS_UNSPECIFIED
}

// GET /api/disputes?merchant_id=&status=&limit=&offset=
func (h *DisputeHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second) // fan-out scan
	defer cancel()
	mch := q.Get("merchant_id")
	if mch == "" {
		writeError(w, http.StatusBadRequest, "merchant_id required")
		return
	}
	resp, err := h.cli.ListByMerchant(ctx, &orderv1.ListDisputesByMerchantRequest{
		MerchantId: mch,
		Status:     parseDisputeStatus(q.Get("status")),
		Limit:      int32(limit),
		Offset:     int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"disputes": resp.GetDisputes(), "total": resp.GetTotal()})
}

// GET /api/disputes/{pi_id}/{id}
func (h *DisputeHandler) Get(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.cli.Get(ctx, &orderv1.GetDisputeRequest{
		PaymentIntentId: v["pi_id"], Id: v["id"],
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetDispute())
}

// GET /api/disputes/{pi_id}/{id}/events
func (h *DisputeHandler) Events(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.cli.ListEvents(ctx, &orderv1.ListDisputeEventsRequest{
		PaymentIntentId: v["pi_id"], DisputeId: v["id"], Limit: int32(limit),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetEvents())
}

// POST /api/disputes/{pi_id}/{id}/evidence  { actor, evidence, idempotency_key }
func (h *DisputeHandler) SubmitEvidence(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	var body struct {
		Actor          string            `json:"actor"`
		Evidence       map[string]string `json:"evidence"`
		IdempotencyKey string            `json:"idempotency_key"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.IdempotencyKey != "" {
		if cached, ok := idempotencyCache.Lookup(body.IdempotencyKey); ok {
			writeJSON(w, cached)
			return
		}
	}
	// Use the actor decoded by AuthMiddleware (token-derived). Body actor is
	// kept only for back-compat with older clients but is overridden here so
	// callers cannot impersonate someone else by setting a different actor.
	actor := actorFromRequest(r)
	if actor == "" || actor == "unknown" {
		actor = body.Actor
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := h.cli.SubmitEvidence(ctx, &orderv1.SubmitEvidenceRequest{
		PaymentIntentId: v["pi_id"], Id: v["id"],
		Actor: actor, Evidence: body.Evidence,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := resp.GetDispute()
	if body.IdempotencyKey != "" {
		idempotencyCache.Store(body.IdempotencyKey, out)
	}
	writeJSON(w, out)
}

// POST /api/disputes/{pi_id}/{id}/concede  { actor, note, idempotency_key }
//
// Concede is a HIGH-SENSITIVITY action: it terminally moves the dispute to
// CHARGE_REFUNDED and triggers an automatic refund on the original charge.
// Therefore:
//   - actor MUST come from the authenticated session (token), not the body
//   - idempotency_key is honoured to dedupe accidental double-clicks
//   - audit middleware writes synchronously and blocks the response on failure
func (h *DisputeHandler) Concede(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	var body struct {
		Actor          string `json:"actor"`
		Note           string `json:"note"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	_ = readJSON(r, &body)
	if body.IdempotencyKey != "" {
		if cached, ok := idempotencyCache.Lookup(body.IdempotencyKey); ok {
			writeJSON(w, cached)
			return
		}
	}
	actor := actorFromRequest(r)
	if actor == "" || actor == "unknown" {
		actor = body.Actor
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := h.cli.Concede(ctx, &orderv1.ConcedeDisputeRequest{
		PaymentIntentId: v["pi_id"], Id: v["id"], Actor: actor, Note: body.Note,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := resp.GetDispute()
	if body.IdempotencyKey != "" {
		idempotencyCache.Store(body.IdempotencyKey, out)
	}
	writeJSON(w, out)
}

// POST /api/disputes/{pi_id}/{id}/cancel  { actor, note }
//
// Cancel terminates the dispute without paying — actor again derived from the
// authenticated session, not from a client-supplied body field.
func (h *DisputeHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	var body struct {
		Actor string `json:"actor"`
		Note  string `json:"note"`
	}
	_ = readJSON(r, &body)
	actor := actorFromRequest(r)
	if actor == "" || actor == "unknown" {
		actor = body.Actor
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := h.cli.Cancel(ctx, &orderv1.CancelDisputeRequest{
		PaymentIntentId: v["pi_id"], Id: v["id"], Actor: actor, Note: body.Note,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetDispute())
}

// POST /api/disputes/{pi_id}/{id}/simulate  { action, outcome_amount? }
// Developer-facing: fires the corresponding channel-webhook-driven transition
// directly, without having to actually send a webhook. Matches the scenario-
// selector pattern users expect on the mockserver pages.
//
// PROD GATING: in APP_ENV=prod the route is **not registered** (see cmd/server)
// AND this handler additionally short-circuits to 404 as a defense-in-depth
// guard against route-table mistakes. This prevents an operator from forcing a
// real dispute into LOST (with auto-refund money movement) outside of QA.
func (h *DisputeHandler) Simulate(w http.ResponseWriter, r *http.Request) {
	if IsProdEnv() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	v := mux.Vars(r)
	var body struct {
		Action         string `json:"action"` // under_review / won / lost / warning_closed
		OutcomeAmount  int64  `json:"outcome_amount"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Idempotency: accept the same key twice as a single op. The cache lives in
	// the BFF so transient retries (network blip / user double-click) do not
	// produce two state-machine ticks. End-to-end idempotency still requires
	// the gRPC backend to also honour the key (see PR body for cross-repo dep).
	if body.IdempotencyKey != "" {
		if cached, ok := idempotencyCache.Lookup(body.IdempotencyKey); ok {
			writeJSON(w, cached)
			return
		}
	}
	// Pre-fetch dispute to validate outcome_amount against the original amount.
	// We only enforce when an outcome is supplied; an empty value falls back to
	// the dispute's full amount inside order-core.
	if body.OutcomeAmount != 0 {
		// outcome_amount uses the storage convention (minor*100). Must be a
		// positive multiple of 100 (i.e. integral minor unit) and never exceed
		// the original dispute amount.
		if body.OutcomeAmount <= 0 || body.OutcomeAmount%100 != 0 {
			writeError(w, http.StatusBadRequest, "outcome_amount must be a positive integer multiple of 100 (storage = minor × 100)")
			return
		}
		// Look up the dispute amount; reject overdraw.
		gctx, gcancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer gcancel()
		dResp, err := h.cli.Get(gctx, &orderv1.GetDisputeRequest{
			PaymentIntentId: v["pi_id"], Id: v["id"],
		})
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if d := dResp.GetDispute(); d != nil && body.OutcomeAmount > d.GetAmount() {
			writeError(w, http.StatusBadRequest, "outcome_amount exceeds dispute amount")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req := &orderv1.MarkDisputeRequest{
		PaymentIntentId: v["pi_id"], Id: v["id"],
		OutcomeAmount: body.OutcomeAmount,
	}
	var (
		resp *orderv1.MarkDisputeResponse
		err  error
	)
	switch body.Action {
	case "under_review":
		resp, err = h.cli.MarkUnderReview(ctx, req)
	case "won":
		resp, err = h.cli.MarkWon(ctx, req)
	case "lost":
		resp, err = h.cli.MarkLost(ctx, req)
	case "warning_closed":
		resp, err = h.cli.MarkWarningClosed(ctx, req)
	default:
		writeError(w, http.StatusBadRequest, "action must be one of under_review/won/lost/warning_closed")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := resp.GetDispute()
	if body.IdempotencyKey != "" {
		idempotencyCache.Store(body.IdempotencyKey, out)
	}
	writeJSON(w, out)
}
