package mgmt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
)

// Telemetry destinations. Secrets and header values are write-only: no
// response carries them, only the names of those that are set.

const defaultMaxContentBytes = 64 << 10

func (m *Server) telemetryKinds(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, k := range telemetry.Kinds() {
		secrets := k.SecretFields()
		if secrets == nil {
			secrets = []string{}
		}
		out = append(out, map[string]any{"name": k.Name(), "label": k.Label(), "beta": k.Beta(), "secret_fields": secrets})
	}
	writeJSON(w, http.StatusOK, out)
}

// destinationRequest is the body of create, replace and test.
type destinationRequest struct {
	// ID, on a test, names the saved destination whose secrets fill in what
	// the draft leaves out; with nothing else set, the saved one is tested.
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	Enabled   *bool             `json:"enabled"` // default true
	Endpoint  string            `json:"endpoint"`
	TracesURL string            `json:"traces_url"`
	Config    map[string]string `json:"config"`
	// Secrets are the kind's secret fields. A missing or empty value keeps
	// the stored one.
	Secrets map[string]string `json:"secrets"`
	// Headers, when present, is the full list of extra request headers: a
	// stored header not listed is deleted, and one listed without a value
	// keeps its stored value. Absent keeps every stored header.
	Headers *[]struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	ContentMode     string         `json:"content_mode"`      // metadata_only (default) | content
	MaxContentBytes int            `json:"max_content_bytes"` // default 65536, at most 1 MiB
	TeamScope       snapshot.Scope `json:"team_scope"`
	GroupScope      snapshot.Scope `json:"group_scope"`
}

// destination builds the destination a request describes, taking secrets
// it leaves out from stored, which may be nil.
func (req *destinationRequest) destination(stored *snapshot.Destination) (*snapshot.Destination, error) {
	d := &snapshot.Destination{
		Name: strings.TrimSpace(req.Name), Kind: req.Kind, Enabled: req.Enabled == nil || *req.Enabled,
		Endpoint: strings.TrimSpace(req.Endpoint), TracesURL: strings.TrimSpace(req.TracesURL),
		Config: map[string]string{}, Secrets: map[string]string{}, ContentMode: req.ContentMode,
		MaxContentBytes: req.MaxContentBytes, Teams: req.TeamScope, Groups: req.GroupScope,
	}
	if d.Name == "" {
		return nil, errors.New("name is required")
	}
	k := telemetry.Lookup(d.Kind)
	if k == nil {
		return nil, fmt.Errorf("unknown kind %q", d.Kind)
	}
	for key, v := range req.Config {
		if v = strings.TrimSpace(v); v != "" {
			d.Config[key] = v
		}
	}
	old := map[string]string{}
	if stored != nil {
		if stored.Err != "" {
			old = nil // can't be read: everything must be entered again
		} else if stored.Kind == d.Kind {
			old = stored.Secrets
		}
	}
	// Stored values are bound to where they were sent: a new host gets them
	// only when they are entered again.
	moved := stored != nil && !sameHosts(stored, d)
	keep := func(key string) (string, error) {
		v := old[key]
		if v != "" && moved {
			return "", errors.New("the endpoint changed: enter the secret key and header values again")
		}
		return v, nil
	}
	for _, name := range k.SecretFields() {
		v := strings.TrimSpace(req.Secrets[name])
		if v == "" {
			var err error
			if v, err = keep(name); err != nil {
				return nil, err
			}
		}
		if v != "" {
			d.Secrets[name] = v
		}
	}
	for name := range req.Secrets {
		if !slices.Contains(k.SecretFields(), name) {
			return nil, fmt.Errorf("unknown secret %q for kind %s", name, d.Kind)
		}
	}
	if req.Headers == nil {
		for key := range old {
			if strings.HasPrefix(key, snapshot.HeaderSecretPrefix) {
				v, err := keep(key)
				if err != nil {
					return nil, err
				}
				d.Secrets[key] = v
			}
		}
	} else {
		seen := map[string]bool{}
		for _, h := range *req.Headers {
			name := strings.TrimSpace(h.Name)
			if seen[strings.ToLower(name)] {
				return nil, fmt.Errorf("header %s is listed twice", name)
			}
			seen[strings.ToLower(name)] = true
			key := snapshot.HeaderSecretPrefix + name
			v := strings.TrimSpace(h.Value)
			if v == "" {
				from := key // the stored header of this name, in any case
				for k := range old {
					if strings.EqualFold(k, key) {
						from = k
					}
				}
				var err error
				if v, err = keep(from); err != nil {
					return nil, err
				}
			}
			if v == "" {
				return nil, fmt.Errorf("header %s needs a value", name)
			}
			d.Secrets[key] = v
		}
	}
	if stored != nil && stored.Err != "" {
		for _, name := range stored.SecretNames() {
			_, kept := d.Secrets[name]
			header := strings.HasPrefix(name, snapshot.HeaderSecretPrefix)
			if !kept && (!header || req.Headers == nil) {
				return nil, errors.New("the stored secrets can't be decrypted: enter them again")
			}
		}
	}
	switch d.ContentMode {
	case "":
		d.ContentMode = "metadata_only"
	case "metadata_only", "content":
	default:
		return nil, errors.New("content_mode must be metadata_only or content")
	}
	if d.MaxContentBytes == 0 {
		d.MaxContentBytes = defaultMaxContentBytes
	}
	if d.MaxContentBytes < 1 || d.MaxContentBytes > 1<<20 {
		return nil, errors.New("max_content_bytes must be 1 to 1048576")
	}
	for _, s := range []*snapshot.Scope{&d.Teams, &d.Groups} {
		if s.Mode == "" {
			s.Mode = "all"
		}
		if s.Mode != "all" && s.Mode != "selected" && s.Mode != "except" {
			return nil, errors.New("scope mode must be all, selected or except")
		}
		vals := []string{}
		for _, v := range s.Values {
			if v = strings.TrimSpace(v); v != "" && !slices.Contains(vals, v) {
				vals = append(vals, v)
			}
		}
		s.Values = vals
	}
	if err := k.Validate(d); err != nil {
		return nil, err
	}
	return d, nil
}

func (m *Server) createDestination(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[destinationRequest](w, r)
	if !ok {
		return
	}
	d, err := req.destination(nil)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	enc, names, err := m.sealSecrets(d)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := uuid.New()
	cfg, _ := json.Marshal(d.Config)
	_, err = m.Store.Pool.Exec(r.Context(), `INSERT INTO telemetry_destinations
		(id, name, kind, enabled, endpoint, traces_url, config, secrets_enc, secret_names, content_mode,
		 max_content_bytes, team_scope, team_values, group_scope, group_values)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		id, d.Name, d.Kind, d.Enabled, d.Endpoint, d.TracesURL, cfg, enc, names, d.ContentMode,
		d.MaxContentBytes, d.Teams.Mode, d.Teams.Values, d.Groups.Mode, d.Groups.Values)
	if err != nil {
		destinationWriteError(w, err)
		return
	}
	m.bump(r.Context())
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

func (m *Server) putDestination(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	req, ok := decode[destinationRequest](w, r)
	if !ok {
		return
	}
	stored, err := m.loadDestination(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d, err := req.destination(stored)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	enc, names, err := m.sealSecrets(d)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, _ := json.Marshal(d.Config)
	tag, err := m.Store.Pool.Exec(r.Context(), `UPDATE telemetry_destinations SET
		name=$2, kind=$3, enabled=$4, endpoint=$5, traces_url=$6, config=$7, secrets_enc=$8, secret_names=$9,
		content_mode=$10, max_content_bytes=$11, team_scope=$12, team_values=$13, group_scope=$14,
		group_values=$15, updated_at=now()
		WHERE id=$1`,
		id, d.Name, d.Kind, d.Enabled, d.Endpoint, d.TracesURL, cfg, enc, names, d.ContentMode,
		d.MaxContentBytes, d.Teams.Mode, d.Teams.Values, d.Groups.Mode, d.Groups.Values)
	if err != nil {
		destinationWriteError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func destinationWriteError(w http.ResponseWriter, err error) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		httpError(w, http.StatusConflict, "a destination with this name already exists")
		return
	}
	httpError(w, http.StatusBadRequest, err.Error())
}

// sealSecrets encrypts the destination's secrets for storage.
func (m *Server) sealSecrets(d *snapshot.Destination) ([]byte, []string, error) {
	names := d.SecretNames()
	if len(names) == 0 {
		return nil, names, nil
	}
	if m.EncryptionKey == "" {
		return nil, nil, errors.New("LLMR_ENCRYPTION_KEY is not set, so secrets can't be stored")
	}
	plain, _ := json.Marshal(d.Secrets)
	enc, err := snapshot.Encrypt(m.EncryptionKey, string(plain))
	return enc, names, err
}

func (m *Server) deleteDestination(w http.ResponseWriter, r *http.Request) {
	m.deleteRow("telemetry_destinations")(w, r)
}

// loadDestination reads a saved destination with its secrets decrypted; one
// whose secrets can't be decrypted comes back with Err set.
func (m *Server) loadDestination(ctx context.Context, id uuid.UUID) (*snapshot.Destination, error) {
	d := &snapshot.Destination{ID: id.String()}
	var enc []byte
	var names []string
	err := m.Store.Pool.QueryRow(ctx, `
		SELECT name, kind, enabled, endpoint, traces_url, config, secrets_enc, secret_names, content_mode,
		       max_content_bytes, team_scope, team_values, group_scope, group_values
		FROM telemetry_destinations WHERE id=$1`, id).Scan(&d.Name, &d.Kind, &d.Enabled, &d.Endpoint,
		&d.TracesURL, &d.Config, &enc, &names, &d.ContentMode, &d.MaxContentBytes, &d.Teams.Mode,
		&d.Teams.Values, &d.Groups.Mode, &d.Groups.Values)
	if err != nil {
		return nil, err
	}
	d.Secrets = map[string]string{}
	if len(enc) > 0 {
		plain, err := snapshot.Decrypt(m.EncryptionKey, enc)
		if err == nil {
			err = json.Unmarshal([]byte(plain), &d.Secrets)
		}
		if err != nil {
			d.Err = "secrets can't be decrypted with LLMR_ENCRYPTION_KEY: " + err.Error()
			d.Secrets = map[string]string{}
			for _, n := range names {
				d.Secrets[n] = "" // keeps the names for SecretNames
			}
		}
	}
	return d, nil
}

func (m *Server) testDestination(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[destinationRequest](w, r)
	if !ok {
		return
	}
	var stored *snapshot.Destination
	if req.ID != "" {
		id, err := uuid.Parse(req.ID)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid id")
			return
		}
		if stored, err = m.loadDestination(r.Context(), id); errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "not found")
			return
		} else if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	d := stored
	if d == nil || req.Name != "" || req.Kind != "" {
		var err error
		if d, err = req.destination(stored); err != nil {
			writeJSON(w, http.StatusOK, telemetry.TestResult{Detail: err.Error()})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, telemetry.Test(ctx, d))
}

func (m *Server) listDestinations(w http.ResponseWriter, r *http.Request) {
	out, err := m.destinations(r.Context(), "")
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *Server) getDestination(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	out, err := m.destinations(r.Context(), id.String())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(out) == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, out[0])
}

// destinationView is a destination as the API shows it: no secret values.
type destinationView struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Kind            string            `json:"kind"`
	Enabled         bool              `json:"enabled"`
	Endpoint        string            `json:"endpoint"`
	TracesURL       string            `json:"traces_url"`
	Config          map[string]string `json:"config"`
	SecretNames     []string          `json:"secret_names"` // the kind's secret fields that are set
	Headers         []string          `json:"headers"`      // names of the extra headers
	ContentMode     string            `json:"content_mode"`
	MaxContentBytes int               `json:"max_content_bytes"`
	TeamScope       snapshot.Scope    `json:"team_scope"`
	GroupScope      snapshot.Scope    `json:"group_scope"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Health          telemetry.Health  `json:"health"`
	// Error is why the destination can't run, e.g. undecryptable secrets.
	Error string `json:"error,omitempty"`
	// TraceURL links to a trace with {trace_id} in it; "" = no link.
	TraceURL string `json:"trace_url"`
}

func (m *Server) destinations(ctx context.Context, id string) ([]destinationView, error) {
	q := `SELECT id::text, name, kind, enabled, endpoint, traces_url, config, secret_names, content_mode,
		max_content_bytes, team_scope, team_values, group_scope, group_values, created_at, updated_at,
		last_success_at, coalesce(last_error, ''), last_error_at
		FROM telemetry_destinations`
	args := []any{}
	if id != "" {
		q += ` WHERE id=$1`
		args = append(args, id)
	}
	rows, err := m.Store.Pool.Query(ctx, q+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []destinationView{}
	for rows.Next() {
		var v destinationView
		var names []string
		var lastOK, lastErrAt *time.Time
		var lastErr string
		if err := rows.Scan(&v.ID, &v.Name, &v.Kind, &v.Enabled, &v.Endpoint, &v.TracesURL, &v.Config, &names,
			&v.ContentMode, &v.MaxContentBytes, &v.TeamScope.Mode, &v.TeamScope.Values, &v.GroupScope.Mode,
			&v.GroupScope.Values, &v.CreatedAt, &v.UpdatedAt, &lastOK, &lastErr, &lastErrAt); err != nil {
			return nil, err
		}
		v.SecretNames, v.Headers = []string{}, []string{}
		for _, n := range names {
			if h, ok := strings.CutPrefix(n, snapshot.HeaderSecretPrefix); ok {
				v.Headers = append(v.Headers, h)
			} else {
				v.SecretNames = append(v.SecretNames, n)
			}
		}
		if v.Config == nil {
			v.Config = map[string]string{}
		}
		v.Health = telemetry.Health{ID: v.ID, Name: v.Name, Kind: v.Kind, LastSuccessAt: lastOK,
			LastError: lastErr, LastErrorAt: lastErrAt}
		if m.Telemetry != nil {
			if h, ok := m.Telemetry.HealthOf(v.ID); ok {
				// The row holds what an earlier process last saw.
				if h.LastSuccessAt == nil {
					h.LastSuccessAt = lastOK
				}
				if h.LastErrorAt == nil {
					h.LastError, h.LastErrorAt = lastErr, lastErrAt
				}
				v.Health = h
			}
		}
		if v.Health.Status == "" {
			v.Health.Status = statusOf(v.Enabled, lastOK, lastErrAt)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		v := &out[i]
		if s := m.Snapshots.Get(); s != nil {
			for _, d := range s.Destinations {
				if d.ID == v.ID {
					v.Error = d.Err
				}
			}
		}
		if m.Telemetry != nil {
			tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			v.TraceURL = m.Telemetry.TraceURL(tctx, v.ID)
			cancel()
		}
	}
	return out, nil
}

// sameHosts reports whether d sends where stored did: the same scheme and
// host for the endpoint and for the traces URL (the endpoint when unset).
func sameHosts(stored, d *snapshot.Destination) bool {
	traces := func(x *snapshot.Destination) string {
		if x.TracesURL != "" {
			return x.TracesURL
		}
		return x.Endpoint
	}
	return origin(stored.Endpoint) == origin(d.Endpoint) && origin(traces(stored)) == origin(traces(d))
}

func origin(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// statusOf is a destination's status from its row alone.
func statusOf(enabled bool, lastOK, lastErrAt *time.Time) string {
	switch {
	case !enabled:
		return "disabled"
	case lastErrAt != nil && (lastOK == nil || lastErrAt.After(*lastOK)):
		return "failing"
	case lastOK == nil:
		return "no_data"
	}
	return "healthy"
}

// removeFromScopes takes a deleted team or gateway group out of every
// destination's scope.
func (m *Server) removeFromScopes(ctx context.Context, column, value string) {
	m.Store.Pool.Exec(ctx, `UPDATE telemetry_destinations SET `+column+` = array_remove(`+column+`, $1)
		WHERE $1 = ANY(`+column+`)`, value)
}
