package mgmt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"

	"github.com/google/uuid"
)

func (m *Server) listEdgeNodes(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT id, name, last_seen_at, last_seen_version, created_at FROM edge_nodes ORDER BY created_at`)
}

func (m *Server) createEdgeNode(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Name string `json:"name"`
	}](w, r)
	if !ok {
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := "llmrn_" + base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(token))
	id := uuid.New()
	_, err := m.Store.Pool.Exec(r.Context(),
		`INSERT INTO edge_nodes (id, name, token_hash) VALUES ($1,$2,$3)`, id, req.Name, hash[:])
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	// token is returned exactly once
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String(), "token": token})
}
