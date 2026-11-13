package mgmt

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

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
	if err := checkIssuerURL(req.IssuerURL); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
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
	auditTarget(r, "token_issuer", id.String())
	m.bump(r.Context())
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

func (m *Server) patchTokenIssuer(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	fields, ok := decodeFields(w, r)
	if !ok {
		return
	}
	if raw, ok := fields["issuer_url"]; ok {
		var u string
		if err := json.Unmarshal(raw, &u); err != nil {
			httpError(w, http.StatusBadRequest, "issuer_url must be a string")
			return
		}
		if err := checkIssuerURL(u); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !m.applyPatch(w, r, "token_issuers", id, tokenIssuerPatchCols, fields) {
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// checkIssuerURL: the control plane and every gateway fetch the keys that
// identity tokens are verified with from here, so it must be https; plain
// http only to this machine, for a local test issuer.
func checkIssuerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("issuer_url %q is not an absolute URL", raw)
	}
	switch h := u.Hostname(); {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1"):
		return nil
	}
	return fmt.Errorf("issuer_url %q must use https (http only for localhost)", raw)
}
