package mgmt

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssuerURLMustBeHTTPS(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://login.microsoftonline.com/t/v2.0": true,
		"https://accounts.google.com":              true,
		"http://localhost:5556/dex":                true,
		"http://127.0.0.1:8080":                    true,
		"http://[::1]:8080":                        true,
		"http://accounts.google.com":               false,
		"http://127.0.0.1.evil.example":            false,
		"ftp://idp.example":                        false,
		"idp.example":                              false,
		"https://":                                 false,
	} {
		if err := checkIssuerURL(raw); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok=%v", raw, err, ok)
		}
	}

	// Both ways in refuse it before touching the database.
	m := &Server{}
	for name, call := range map[string]func() *httptest.ResponseRecorder{
		"create": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			m.createTokenIssuer(rec, httptest.NewRequest("POST", "/api/token-issuers", strings.NewReader(
				`{"type":"oidc","issuer_url":"http://idp.example","audience":"llmr"}`)))
			return rec
		},
		"patch": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("PATCH", "/api/token-issuers/x", strings.NewReader(`{"issuer_url":"http://idp.example"}`))
			req.SetPathValue("id", "6f0c7d1e-2a7b-4a52-9d55-0e7e1c2b8f10")
			m.patchTokenIssuer(rec, req)
			return rec
		},
	} {
		rec := call()
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "https") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}
