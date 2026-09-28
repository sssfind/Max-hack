package handler

import (
	"Max-hack/internal/maxapi"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type acceptorStub struct {
	updates []maxapi.Update
	err     error
}

func (a *acceptorStub) Accept(_ context.Context, update maxapi.Update) error {
	a.updates = append(a.updates, update)
	return a.err
}

func TestWebhookPersistsBeforeAcknowledgement(t *testing.T) {
	acceptor := &acceptorStub{}
	h := NewWebhookHandler(acceptor)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{
		"update_type":"message_created",
		"timestamp":42,
		"message":{"recipient":{"chat_id":77},"body":{"mid":"m-1","text":"hello"}}
	}`))
	recorder := httptest.NewRecorder()

	h.Handle(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if len(acceptor.updates) != 1 {
		t.Fatalf("accepted updates = %d, want 1", len(acceptor.updates))
	}
	if got := acceptor.updates[0].ResolvedChatID(); got != 77 {
		t.Fatalf("chat id = %d, want 77", got)
	}
}

func TestWebhookReturnsServiceUnavailableWhenPersistenceFails(t *testing.T) {
	acceptor := &acceptorStub{err: errors.New("database unavailable")}
	h := NewWebhookHandler(acceptor)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"update_type":"bot_started","timestamp":42}`))
	recorder := httptest.NewRecorder()

	h.Handle(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if len(acceptor.updates) != 1 {
		t.Fatalf("accepted updates = %d, want 1 persistence attempt", len(acceptor.updates))
	}
}

func TestWebhookRejectsOversizedBody(t *testing.T) {
	acceptor := &acceptorStub{}
	h := NewWebhookHandlerWithLimit(acceptor, 32)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"update_type":"message_created","padding":"too long for configured limit"}`))
	recorder := httptest.NewRecorder()

	h.Handle(recorder, req)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
	if len(acceptor.updates) != 0 {
		t.Fatalf("oversized body reached persistence: %d updates", len(acceptor.updates))
	}
}

func TestWebhookRejectsMalformedOrTrailingJSON(t *testing.T) {
	tests := []string{
		`{"update_type":`,
		`{"update_type":"bot_started"} {"update_type":"bot_stopped"}`,
		`null`,
		`{}`,
	}
	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			acceptor := &acceptorStub{}
			h := NewWebhookHandler(acceptor)
			req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
			recorder := httptest.NewRecorder()

			h.Handle(recorder, req)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if len(acceptor.updates) != 0 {
				t.Fatalf("invalid body reached persistence: %d updates", len(acceptor.updates))
			}
		})
	}
}

func TestWebhookSecretMiddleware(t *testing.T) {
	t.Parallel()

	called := false
	handler := WebhookSecretMiddleware("expected-secret", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	for _, secret := range []string{"", "wrong", "expected-secret-extra"} {
		called = false
		req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
		if secret != "" {
			req.Header.Set("X-Max-Bot-Api-Secret", secret)
		}
		recorder := httptest.NewRecorder()
		handler(recorder, req)
		if recorder.Code != http.StatusUnauthorized || called {
			t.Fatalf("secret %q: status=%d called=%t", secret, recorder.Code, called)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Max-Bot-Api-Secret", "expected-secret")
	recorder := httptest.NewRecorder()
	handler(recorder, req)
	if recorder.Code != http.StatusNoContent || !called {
		t.Fatalf("valid secret: status=%d called=%t", recorder.Code, called)
	}
}
