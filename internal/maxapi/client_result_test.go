package maxapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnswerCallbackTreatsSuccessFalseAsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/answers" || r.URL.Query().Get("callback_id") != "callback-1" {
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":false,"message":"callback expired"}`)
	}))
	t.Cleanup(server.Close)
	client := newClient("token", server.URL, server.Client())

	err := client.AnswerCallback(context.Background(), "callback-1", 42, SendAnswerRequest{})
	if err == nil || !strings.Contains(err.Error(), "callback expired") {
		t.Fatalf("AnswerCallback error = %v", err)
	}
}

func TestSubscribeTreatsEmptySuccessFalseAsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subscriptions" {
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":false}`)
	}))
	t.Cleanup(server.Close)
	client := newClient("token", server.URL, server.Client())

	err := client.Subscribe(context.Background(), "https://bot.example/webhook", "secret", []string{UpdateMessageCreated})
	if err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Fatalf("Subscribe error = %v", err)
	}
}

func TestRetryableDeliveryErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "rate limited", err: &APIError{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "server unavailable", err: &APIError{StatusCode: http.StatusServiceUnavailable}, want: true},
		{name: "expired callback", err: &APIError{StatusCode: http.StatusBadRequest}, want: false},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "application result", err: errors.New("success=false"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryableDeliveryError(tt.err); got != tt.want {
				t.Fatalf("IsRetryableDeliveryError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
