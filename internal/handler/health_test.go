package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type readinessStub struct {
	err error
}

func (p readinessStub) Ready(context.Context) error { return p.err }

func TestHealth(t *testing.T) {
	recorder := httptest.NewRecorder()
	Health(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !strings.Contains(recorder.Body.String(), `"status":"ok"`) {
		t.Fatalf("unexpected body: %q", recorder.Body.String())
	}
}

func TestReadiness(t *testing.T) {
	tests := []struct {
		name       string
		checker    readinessStub
		wantStatus int
		wantBody   string
	}{
		{name: "ready", checker: readinessStub{}, wantStatus: http.StatusOK, wantBody: `"status":"ready"`},
		{name: "database unavailable", checker: readinessStub{err: errors.New("down")}, wantStatus: http.StatusServiceUnavailable, wantBody: `"status":"not_ready"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			Readiness(tt.checker, time.Second)(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if !strings.Contains(recorder.Body.String(), tt.wantBody) {
				t.Fatalf("unexpected body: %q", recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("content type = %q, want application/json", got)
			}
		})
	}
}

func TestCombinedReadinessRequiresEveryDependency(t *testing.T) {
	checker := CombineReadiness(readinessStub{}, readinessStub{err: errors.New("sessions unavailable")})
	recorder := httptest.NewRecorder()
	Readiness(checker, time.Second)(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}
