package mgmt

import (
	"net/http"
	"testing"
)

func TestIDEProxyStripsCallerCredentials(t *testing.T) {
	h := http.Header{}
	h.Set("Cookie", "llmr_session=abc")
	h.Set("Authorization", "Bearer llmr_x")
	h.Set("X-Llmr-Key", "llmr_y")
	h.Set("x-api-key", "sk-z")
	h.Set("Accept", "text/html")
	stripCallerCredentials(h)
	for _, name := range []string{"Cookie", "Authorization", "X-Llmr-Key", "X-Api-Key"} {
		if h.Get(name) != "" {
			t.Errorf("%s reached the container", name)
		}
	}
	if h.Get("Accept") != "text/html" {
		t.Error("unrelated headers must pass")
	}
}
