package gateway

import (
	"encoding/json"
	"net/http"
)

// writeError emits an OpenAI-compatible error body.
func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
}

func writeAuthError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", message)
}
