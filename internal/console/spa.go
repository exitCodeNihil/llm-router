package console

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/exitcodenihil/llm-router/web"
)

// SPA serves the embedded console. Unknown paths fall back to index.html so
// client-side routing works.
func SPA() http.Handler {
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// unknown API-ish routes must 404, not serve the SPA shell
		for _, prefix := range []string{"/api/", "/v1/", "/auth/", "/edge/"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"not found"}`))
				return
			}
		}
		if r.URL.Path != "/" {
			if _, err := fs.Stat(dist, r.URL.Path[1:]); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFileFS(w, r, dist, "index.html")
	})
}
