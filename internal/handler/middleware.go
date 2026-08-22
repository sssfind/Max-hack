package handler

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
)

func WebhookSecretMiddleware(expectedSecret string, next http.HandlerFunc) http.HandlerFunc {
	expectedBytes := []byte(expectedSecret)

	return func(w http.ResponseWriter, r *http.Request) {
		incomingSecret := r.Header.Get("X-Max-Bot-Api-Secret")
		if incomingSecret == "" {
			slog.Warn("Missing webhook secret", "ip", r.RemoteAddr)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		incomingBytes := []byte(incomingSecret)
		if subtle.ConstantTimeCompare(incomingBytes, expectedBytes) != 1 {
			slog.Warn("Invalid webhook secret", "ip", r.RemoteAddr)
			// Отдаем строго 401 Unauthorized при несовпадении
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	}
}
