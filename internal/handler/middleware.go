package handler

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
)

func WebhookSecretMiddleware(expectedSecret string, next http.HandlerFunc) http.HandlerFunc {
	expectedHash := sha256.Sum256([]byte(expectedSecret))

	return func(w http.ResponseWriter, r *http.Request) {
		incomingSecret := r.Header.Get("X-Max-Bot-Api-Secret")
		if incomingSecret == "" {
			slog.Warn("Missing webhook secret")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		incomingHash := sha256.Sum256([]byte(incomingSecret))
		if subtle.ConstantTimeCompare(incomingHash[:], expectedHash[:]) != 1 {
			slog.Warn("Invalid webhook secret")
			// Отдаем строго 401 Unauthorized при несовпадении
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	}
}
