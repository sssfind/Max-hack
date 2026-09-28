package handler

import (
	"Max-hack/internal/maxapi"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const DefaultMaxWebhookBodyBytes int64 = 1 << 20

type updateAcceptor interface {
	Accept(context.Context, maxapi.Update) error
}

type WebhookHandler struct {
	acceptor    updateAcceptor
	maxBodySize int64
}

func NewWebhookHandler(acceptor updateAcceptor) *WebhookHandler {
	return NewWebhookHandlerWithLimit(acceptor, DefaultMaxWebhookBodyBytes)
}

func NewWebhookHandlerWithLimit(acceptor updateAcceptor, maxBodySize int64) *WebhookHandler {
	if maxBodySize <= 0 {
		maxBodySize = DefaultMaxWebhookBodyBytes
	}
	return &WebhookHandler{acceptor: acceptor, maxBodySize: maxBodySize}
}

func (h *WebhookHandler) Handle(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodySize)
	decoder := json.NewDecoder(r.Body)
	var update maxapi.Update
	if err := decoder.Decode(&update); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
			return
		}
		slog.Error("Failed to decode webhook payload", "error", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// A webhook request must contain exactly one JSON object. Ignoring trailing
	// data would make the persisted idempotency key ambiguous.
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(update.UpdateType) == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Accept persists the update before this endpoint acknowledges it. A 5xx
	// asks MAX to retry when PostgreSQL is unavailable.
	if err := h.acceptor.Accept(r.Context(), update); err != nil {
		slog.Error("Failed to persist webhook payload", "error", err, "type", update.UpdateType)
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
}
