package mgmt

import (
	"net/http"

	"github.com/google/uuid"
)

var tokenIssuerPatchCols = []string{"issuer_url", "audience", "claim_mapping", "enabled"}

func (m *Server) listTokenIssuers(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT id, type, issuer_url, audience, claim_mapping, enabled, created_at FROM token_issuers ORDER BY created_at`)
}

func (m *Server) createTokenIssuer(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Type         string         `json:"type"` // entra | gcp
		IssuerURL    string         `json:"issuer_url"`
		Audience     string         `json:"audience"`
		ClaimMapping map[string]any `json:"claim_mapping"`
	}](w, r)
	if !ok {
		return
	}
	if req.Type == "" || req.IssuerURL == "" || req.Audience == "" {
		httpError(w, http.StatusBadRequest, "type, issuer_url, audience are required")
		return
	}
	if req.ClaimMapping == nil {
		req.ClaimMapping = map[string]any{}
	}
	id := uuid.New()
	_, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO token_issuers (id, type, issuer_url, audience, claim_mapping)
		VALUES ($1,$2,$3,$4,$5)`,
		id, req.Type, req.IssuerURL, req.Audience, req.ClaimMapping)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.bump(r.Context())
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}
