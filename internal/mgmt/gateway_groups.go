package mgmt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/gwsync"
)

var gatewayGroupPatchCols = []string{"name", "region", "allowed_teams", "data_rule"}

func newJoinToken() (string, []byte) {
	b := make([]byte, 32)
	rand.Read(b)
	token := gwsync.JoinTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(token))
	return token, hash[:]
}

func (m *Server) listGatewayGroups(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT g.id, g.name, g.region, g.allowed_teams, g.data_rule, g.created_at,
			count(i.id) FILTER (WHERE i.last_seen_at > now() - interval '1 minute') AS live_instances
		FROM gateway_groups g LEFT JOIN gateway_instances i ON i.group_id = g.id
		GROUP BY g.id ORDER BY g.created_at`)
}

func (m *Server) createGatewayGroup(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Name         string   `json:"name"`
		Region       string   `json:"region"`
		AllowedTeams []string `json:"allowed_teams"`
		DataRule     string   `json:"data_rule"`
	}](w, r)
	if !ok {
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.DataRule == "" {
		req.DataRule = "inherit"
	}
	token, hash := newJoinToken()
	id := uuid.New()
	if _, err := m.Store.Pool.Exec(r.Context(),
		`INSERT INTO gateway_groups (id, name, region, allowed_teams, data_rule, join_token_hash) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, req.Name, req.Region, req.AllowedTeams, req.DataRule, hash); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.bump(r.Context())
	// the join token is returned exactly once
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String(), "join_token": token})
}

// rotateGatewayToken replaces the join token. Running gateways keep their
// sessions; new ones need the new token.
func (m *Server) rotateGatewayToken(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	token, hash := newJoinToken()
	tag, err := m.Store.Pool.Exec(r.Context(), `UPDATE gateway_groups SET join_token_hash=$2 WHERE id=$1`, id, hash)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"join_token": token})
}

// deleteGatewayGroup removes a group and its instances. Its gateways stop
// getting config at once and their sessions end within minutes.
func (m *Server) deleteGatewayGroup(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	tag, err := m.Store.Pool.Exec(r.Context(), `DELETE FROM gateway_groups WHERE id=$1`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (m *Server) listGatewayInstances(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT i.id, i.group_id, g.name AS group_name, g.region, i.name, i.version, i.joined_at, i.last_seen_at
		FROM gateway_instances i JOIN gateway_groups g ON g.id = i.group_id
		ORDER BY g.name, i.joined_at`)
}
