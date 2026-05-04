package handler

import (
	"context"
	"net/http"
	"time"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

type KMSHandler struct{ deps clients.Deps }

func NewKMSHandler(d clients.Deps) *KMSHandler { return &KMSHandler{deps: d} }

// GET /api/kms/keys
func (h *KMSHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := h.deps.KMS.ListKeys(ctx, &kmsv1.ListKeysRequest{})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	items := make([]map[string]interface{}, 0, len(resp.GetKeys()))
	for _, k := range resp.GetKeys() {
		items = append(items, map[string]interface{}{
			"key_id":     k.GetKeyId(),
			"algorithm":  k.GetAlgorithm(),
			"created_at": k.GetCreatedAt(),
			"active":     k.GetActive(),
		})
	}
	writeJSON(w, map[string]interface{}{
		"items":         items,
		"active_key_id": resp.GetActiveKeyId(),
	})
}

// POST /api/kms/encrypt
// body: { plaintext, context, key_id? }
func (h *KMSHandler) Encrypt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Plaintext string `json:"plaintext"`
		Context   string `json:"context"`
		KeyID     string `json:"key_id"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Plaintext == "" {
		writeError(w, http.StatusBadRequest, "plaintext required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := h.deps.KMS.Encrypt(ctx, &kmsv1.EncryptRequest{
		Plaintext: []byte(body.Plaintext),
		Context:   body.Context,
		KeyId:     body.KeyID,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{
		"ciphertext": resp.GetCiphertext(),
		"key_id":     resp.GetKeyId(),
	})
}

// POST /api/kms/decrypt
// body: { ciphertext, context }
func (h *KMSHandler) Decrypt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ciphertext string `json:"ciphertext"`
		Context    string `json:"context"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Ciphertext == "" {
		writeError(w, http.StatusBadRequest, "ciphertext required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := h.deps.KMS.Decrypt(ctx, &kmsv1.DecryptRequest{
		Ciphertext: body.Ciphertext,
		Context:    body.Context,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{
		"plaintext": string(resp.GetPlaintext()),
		"key_id":    resp.GetKeyId(),
	})
}
