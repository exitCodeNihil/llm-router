package mgmt

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/exitcodenihil/llm-router/internal/console"
)

// Caller is the authenticated management-API user: either the bootstrap admin
// token or a console session.
type Caller struct {
	UserID    string
	Email     string
	IsAdmin   bool              // global admin (users.role='admin') or bootstrap token
	TeamRoles map[string]string // team_id -> 'admin' | 'member'
}

func (c *Caller) TeamAdmin(teamID string) bool {
	return c.IsAdmin || c.TeamRoles[teamID] == "admin"
}

func (c *Caller) InTeam(teamID string) bool {
	_, ok := c.TeamRoles[teamID]
	return c.IsAdmin || ok
}

type callerKeyType int

const callerKey callerKeyType = 0

func CallerFrom(ctx context.Context) *Caller { c, _ := ctx.Value(callerKey).(*Caller); return c }

func (m *Server) resolveCaller(r *http.Request) *Caller {
	if token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); token != "" && m.AdminToken != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(m.AdminToken)) == 1 {
		return &Caller{IsAdmin: true, Email: "bootstrap-admin"}
	}
	userID := console.SessionUserID(m.EncryptionKey, r)
	if userID == "" {
		return nil
	}
	c := &Caller{UserID: userID, TeamRoles: map[string]string{}}
	var role string
	err := m.Store.Pool.QueryRow(r.Context(),
		`SELECT email, role FROM users WHERE id=$1 AND NOT disabled`, userID).Scan(&c.Email, &role)
	if err != nil {
		return nil
	}
	c.IsAdmin = role == "admin"
	rows, err := m.Store.Pool.Query(r.Context(),
		`SELECT team_id, role FROM team_members WHERE user_id=$1`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var teamID, teamRole string
		if rows.Scan(&teamID, &teamRole) == nil {
			c.TeamRoles[teamID] = teamRole
		}
	}
	return c
}

// requireAuth admits any authenticated caller; requireAdmin only global admins.
func (m *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := m.resolveCaller(r)
		if c == nil {
			httpError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), callerKey, c)))
	})
}
