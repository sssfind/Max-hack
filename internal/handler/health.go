package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type readinessChecker interface {
	Ready(context.Context) error
}

type readinessGroup []readinessChecker

func (g readinessGroup) Ready(ctx context.Context) error {
	for _, checker := range g {
		if checker == nil {
			continue
		}
		if err := checker.Ready(ctx); err != nil {
			return err
		}
	}
	return nil
}

func CombineReadiness(checkers ...readinessChecker) readinessChecker {
	return readinessGroup(checkers)
}

type healthResponse struct {
	Status string `json:"status"`
}

func Health(w http.ResponseWriter, _ *http.Request) {
	writeHealth(w, http.StatusOK, "ok")
}

func Readiness(checker readinessChecker, timeout time.Duration) http.HandlerFunc {
	if timeout <= 0 {
		timeout = time.Second
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if err := checker.Ready(ctx); err != nil {
			writeHealth(w, http.StatusServiceUnavailable, "not_ready")
			return
		}
		writeHealth(w, http.StatusOK, "ready")
	}
}

func writeHealth(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(healthResponse{Status: value})
}
