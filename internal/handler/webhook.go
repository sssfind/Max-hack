package handler

import (
	"Max-hack/internal/maxapi"
	"Max-hack/internal/worker"
	"encoding/json"
	"log/slog"
	"net/http"
)

type WebhookHandler struct {
	pool *worker.Pool
}

func NewWebhookHandler(pool *worker.Pool) *WebhookHandler {
	return &WebhookHandler{pool: pool}
}

func (h *WebhookHandler) Handle(w http.ResponseWriter, r *http.Request) {

	var update maxapi.Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		slog.Error("Failed to decode webhook payload", "error", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	h.pool.Submit(update)

	w.WriteHeader(http.StatusOK)
}
