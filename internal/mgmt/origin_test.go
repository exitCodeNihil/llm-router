package mgmt

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCSRFGuard(t *testing.T) {
	h := CSRFGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	cases := []struct {
		name, method, path, origin, site string
		want                             int
	}{
		{"same origin post", "POST", "/api/keys", "http://console.example", "", 204},
		{"no origin (curl)", "POST", "/api/keys", "", "", 204},
		{"cross origin post", "POST", "/api/keys", "http://console.example:8081", "", 403},
		{"other host", "DELETE", "/api/users/x", "https://evil.example", "", 403},
		{"cross site by fetch metadata", "POST", "/auth/password", "", "cross-site", 403},
		{"get is never blocked", "GET", "/api/me", "https://evil.example", "", 204},
		{"gateway routes are not console routes", "POST", "/v1/chat/completions", "https://evil.example", "", 204},
		{"forwarded host behind a proxy", "POST", "/api/keys", "https://public.example", "public.example", 204},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, "http://console.example"+tc.path, nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if tc.site == "cross-site" {
			req.Header.Set("Sec-Fetch-Site", tc.site)
		} else if tc.site != "" {
			req.Header.Set("X-Forwarded-Host", tc.site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
