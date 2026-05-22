package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"time"
)

type contextKey string

const RequestIDKey contextKey = "request_id"

func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if requestID == "" {
			requestID = generateRequestID(r)
		}
		w.Header().Set("X-Request-Id", requestID)
		ctx := context.WithValue(r.Context(), RequestIDKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func WriteError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	WriteJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   message,
			"retryable": retryable,
		},
	})
}

func generateRequestID(r *http.Request) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(r.Method))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(r.URL.Path))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	return fmt.Sprintf("req_%x", h.Sum64())
}
