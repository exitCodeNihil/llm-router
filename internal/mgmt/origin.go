package mgmt

import (
	"net/http"
	"net/url"
	"strings"
)

// RegisterIDE mounts only the workspace IDE proxy, for the dedicated IDE
// listener. Everything a page inside a workspace can reach on that origin is
// the IDE itself: no management API, no console, no gateway.
func (m *Server) RegisterIDE(mux *http.ServeMux) {
	mux.Handle("/api/workspaces/{id}/ide/", m.requireWorkspaceAccess(m.workspaceIDE))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "this origin serves workspace IDEs only", http.StatusNotFound)
	})
}

// CSRFGuard rejects state-changing console requests whose Origin is not this
// host. Sessions are cookies, and a cookie is sent by any page on the same
// site — including a page rendered inside a workspace on the IDE origin — so
// the browser-supplied Origin is what keeps such a page from acting as you.
// Requests without an Origin header (curl, SDKs) are untouched: they carry
// bearer credentials, which no other site can attach.
func CSRFGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "null" {
			// Behind a reverse proxy that rewrites Host, the public host
			// travels in X-Forwarded-Host; the browser's Origin names that one.
			host := r.Host
			if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
				host = strings.TrimSpace(strings.Split(fh, ",")[0])
			}
			if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, host) {
				httpError(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
		} else if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			httpError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}
