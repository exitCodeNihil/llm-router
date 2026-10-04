package mgmt

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/workspace"
)

// --- templates: the admin-curated menu of workspace images ---

func (m *Server) listWorkspaceTemplates(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT id, name, description, image, dockerfile, source, updated_at
		FROM workspace_templates ORDER BY source DESC, name`)
}

type templateReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Image       string `json:"image"`
	Dockerfile  string `json:"dockerfile"`
}

func (m *Server) createWorkspaceTemplate(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[templateReq](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Image) == "" {
		httpError(w, http.StatusBadRequest, "name and image are required")
		return
	}
	id := uuid.New()
	if _, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO workspace_templates (id, name, description, image, dockerfile, source)
		VALUES ($1, $2, $3, $4, $5, 'admin')`,
		id, strings.TrimSpace(req.Name), req.Description, strings.TrimSpace(req.Image), req.Dockerfile); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

// patchWorkspaceTemplate edits a template. Any edit flips a shipped template to
// source='admin' so the next upgrade's seed leaves it alone.
func (m *Server) patchWorkspaceTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	req, ok := decode[templateReq](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Image) == "" {
		httpError(w, http.StatusBadRequest, "name and image are required")
		return
	}
	tag, err := m.Store.Pool.Exec(r.Context(), `
		UPDATE workspace_templates SET name=$2, description=$3, image=$4, dockerfile=$5, source='admin', updated_at=now()
		WHERE id=$1`, id, strings.TrimSpace(req.Name), req.Description, strings.TrimSpace(req.Image), req.Dockerfile)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "template not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// buildWorkspaceTemplate builds the template's image on the container runtime
// and streams the daemon's output. Only Docker/podman can build; on Kubernetes
// the image is built and pushed by whatever CI the cluster trusts.
func (m *Server) buildWorkspaceTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var image, dockerfile string
	if err := m.Store.Pool.QueryRow(r.Context(),
		`SELECT image, dockerfile FROM workspace_templates WHERE id=$1`, id).Scan(&image, &dockerfile); err != nil {
		httpError(w, http.StatusNotFound, "template not found")
		return
	}
	if strings.TrimSpace(dockerfile) == "" {
		httpError(w, http.StatusBadRequest, "this template has no Dockerfile to build from")
		return
	}
	rt, _, err := m.runtime(r.Context())
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	builder, ok := rt.(workspace.Builder)
	if !ok {
		httpError(w, http.StatusBadRequest,
			"this runtime cannot build images — build the Dockerfile with your CI and push "+image+" to a registry the cluster can pull from")
		return
	}
	send, ok := stream(w)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	err = builder.Build(r.Context(), image, dockerfile, func(line string) error {
		send(jsonLine(map[string]string{"stream": "stdout", "data": line}))
		return nil
	})
	if err != nil {
		send(jsonLine(map[string]string{"stream": "error", "data": err.Error()}))
		return
	}
	send(jsonLine(map[string]string{"stream": "exit", "data": "0"}))
}

// templateImages is the set of images non-admins may create workspaces from.
func (m *Server) templateImages(r *http.Request) map[string]bool {
	out := map[string]bool{}
	rows, err := m.Store.Pool.Query(r.Context(), `SELECT image FROM workspace_templates`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var img string
		if rows.Scan(&img) == nil {
			out[img] = true
		}
	}
	return out
}

// --- per-user files: injected into every workspace the user owns ---

const maxUserFileBytes = 1 << 20 // an ssh key or a .gitconfig, not a dataset

func (m *Server) listUserFiles(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	if c.UserID == "" {
		writeJSON(w, http.StatusOK, []any{}) // a token session owns no files
		return
	}
	m.list(w, r, `SELECT path, mode, size, updated_at FROM workspace_user_files WHERE user_id=$1 ORDER BY path`, c.UserID)
}

// putUserFiles stores uploaded files (multipart, optional "path" directory)
// under the caller's account. They land in $HOME of each of their workspaces
// on every start, so one upload covers every workspace, present and future.
func (m *Server) putUserFiles(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	if c.UserID == "" {
		httpError(w, http.StatusForbidden, "sign in as a user to keep workspace files")
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "invalid upload: "+err.Error())
		return
	}
	dir := strings.Trim(r.FormValue("path"), "/")
	n := 0
	for _, headers := range r.MultipartForm.File {
		for _, fh := range headers {
			f, err := fh.Open()
			if err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			data, err := io.ReadAll(io.LimitReader(f, maxUserFileBytes+1))
			f.Close()
			if err != nil || len(data) > maxUserFileBytes {
				httpError(w, http.StatusBadRequest, fh.Filename+": files are capped at 1 MB")
				return
			}
			target := baseName(fh.Filename)
			if dir != "" {
				target = dir + "/" + target
			}
			if _, err := workspace.SafePath(target); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			enc, err := snapshot.Encrypt(m.EncryptionKey, string(data))
			if err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			if _, err := m.Store.Pool.Exec(r.Context(), `
				INSERT INTO workspace_user_files (user_id, path, mode, data_enc, size)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (user_id, path) DO UPDATE
				SET mode=EXCLUDED.mode, data_enc=EXCLUDED.data_enc, size=EXCLUDED.size, updated_at=now()`,
				c.UserID, target, fileMode(target), enc, len(data)); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			n++
		}
	}
	if n == 0 {
		httpError(w, http.StatusBadRequest, "no files in upload")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"stored": n})
}

func (m *Server) deleteUserFile(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	path := r.URL.Query().Get("path")
	if path == "" {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			path = req.Path
		}
	}
	if path == "" {
		httpError(w, http.StatusBadRequest, "path is required")
		return
	}
	if c.UserID == "" {
		httpError(w, http.StatusForbidden, "sign in as a user to keep workspace files")
		return
	}
	tag, err := m.Store.Pool.Exec(r.Context(), `DELETE FROM workspace_user_files WHERE user_id=$1 AND path=$2`, c.UserID, path)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "no such file")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// userFiles decrypts a user's stored files into $HOME-relative workspace files.
func (m *Server) userFiles(r *http.Request, userID uuid.UUID) ([]workspace.File, error) {
	rows, err := m.Store.Pool.Query(r.Context(),
		`SELECT path, mode, data_enc FROM workspace_user_files WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []workspace.File
	for rows.Next() {
		var path string
		var mode int64
		var enc []byte
		if err := rows.Scan(&path, &mode, &enc); err != nil {
			return nil, err
		}
		data, err := snapshot.Decrypt(m.EncryptionKey, enc)
		if err != nil {
			return nil, err
		}
		// $HOME is .home under the workspace root.
		files = append(files, workspace.File{Path: ".home/" + path, Mode: mode, Data: []byte(data)})
	}
	return files, rows.Err()
}
