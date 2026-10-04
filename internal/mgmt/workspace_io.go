package mgmt

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/exitcodenihil/llm-router/internal/workspace"
)

// --- files ---

// uploadFiles copies files in from the browser — ssh keys, a .gitconfig, a
// certificate. Multipart so several can land at once.
func (m *Server) uploadFiles(w http.ResponseWriter, r *http.Request) {
	ws, ok := m.loadWorkspace(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "invalid upload: "+err.Error())
		return
	}
	dir := r.FormValue("path")
	var files []workspace.File
	for _, headers := range r.MultipartForm.File {
		for _, fh := range headers {
			f, err := fh.Open()
			if err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			data, err := io.ReadAll(io.LimitReader(f, 8<<20))
			f.Close()
			if err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			// Only the base name is used: a browser may send a full path, and
			// the client does not get to choose the directory per file.
			target := strings.TrimPrefix(dir+"/", "/") + baseName(fh.Filename)
			files = append(files, workspace.File{Path: target, Mode: fileMode(target), Data: data})
		}
	}
	if len(files) == 0 {
		httpError(w, http.StatusBadRequest, "no files in upload")
		return
	}
	rt, _, err := m.runtime(r.Context())
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := rt.WriteFiles(r.Context(), sandboxName(ws.ID), files); err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"written": len(files)})
}

func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// fileMode gives ssh material 0600 — ssh silently refuses to use a private key
// that is group- or world-readable, which is a maddening thing to debug.
func fileMode(p string) int64 {
	if strings.Contains(p, "/.ssh/") || strings.HasPrefix(p, ".ssh/") {
		return 0o600
	}
	return 0o644
}

// --- streaming ---

// stream sets up a flushed chunked ndjson response (template image builds).
// The console hand-parses streamed bodies with res.body.getReader(), so this
// needs no new transport and no WebSocket.
func stream(w http.ResponseWriter) (func(string), bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	return func(s string) {
		io.WriteString(w, s)
		fl.Flush()
	}, true
}

func jsonLine(v map[string]string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "\n"
	}
	return string(b) + "\n"
}
