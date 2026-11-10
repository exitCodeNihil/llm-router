package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ValidTraceID reports whether s is a W3C trace id: 32 lowercase hex digits,
// not all zero.
func ValidTraceID(s string) bool { return validHexID(s, 32) }

// ValidSpanID reports whether s is a W3C span id: 16 lowercase hex digits,
// not all zero.
func ValidSpanID(s string) bool { return validHexID(s, 16) }

func validHexID(s string, n int) bool {
	if len(s) != n || strings.Count(s, "0") == n {
		return false
	}
	for i := 0; i < n; i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// TraceIDFor derives a trace id from a request id, for events that arrive
// without one: the same request always gets the same trace.
func TraceIDFor(requestID string) string {
	sum := sha256.Sum256([]byte("llmr-trace:" + requestID))
	return hex.EncodeToString(sum[:16])
}
