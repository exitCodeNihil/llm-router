package mgmt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/audit"
	"github.com/exitcodenihil/llm-router/internal/console"
)

// Every request that is not a GET to a management route is audited, except
// the routes in auditSkip. What a row keeps of the request body is the
// route's auditPolicy: named fields' values, only the names of the fields
// sent, or nothing. A route missing from both maps fails TestAuditCoverage.

// detailPolicy is what of a JSON request body is stored. Neither none nor
// fields: the sorted names of the fields present, without values.
type detailPolicy struct {
	none   bool
	fields []string
}

var (
	noDetail    = detailPolicy{none: true}
	fieldNames  = detailPolicy{}
	keep        = func(f ...string) detailPolicy { return detailPolicy{fields: f} }
	spendFields = []string{"budget_usd", "budget_period", "rpm_limit", "tpm_limit", "allowed_models"}
)

func with(base []string, more ...string) []string {
	return append(append([]string{}, base...), more...)
}

var deploymentAuditFields = []string{"provider_id", "model_name", "upstream_name", "api_flavor", "priority",
	"catalog_model_id", "input_per_1m", "output_per_1m", "cached_input_per_1m", "context_tokens", "region", "timeout_ms"}

// auditPolicy never names a field that holds a credential, an email address,
// a person's name, free text (tags, descriptions, prompts, a Dockerfile) or a
// URL, which can carry credentials of its own.
var auditPolicy = map[string]detailPolicy{
	"POST /api/me/password":         noDetail,
	"POST /api/users/{id}/password": noDetail,

	"POST /api/users":                      keep(with(spendFields, "role")...),
	"PATCH /api/users/{id}":                keep(with(spendFields, "role", "disabled")...),
	"POST /api/users/{id}/erase":           noDetail,
	"DELETE /api/users/{id}":               noDetail,
	"POST /api/teams":                      keep(with(spendFields, "name")...),
	"PATCH /api/teams/{id}":                keep(with(spendFields, "name")...),
	"DELETE /api/teams/{id}":               noDetail,
	"POST /api/teams/{id}/members":         keep("role", "budget_usd", "budget_period"),
	"PATCH /api/teams/{id}/members/{uid}":  keep("role", "budget_usd", "budget_period"),
	"DELETE /api/teams/{id}/members/{uid}": noDetail,
	"POST /api/keys":                       keep(with(spendFields, "name", "user_id", "team_id", "expires_at")...),
	"PATCH /api/keys/{id}":                 keep(with(spendFields, "name", "expires_at", "disabled")...),
	"DELETE /api/keys/{id}":                noDetail,

	"POST /api/providers":          noDetail,
	"PATCH /api/providers/{id}":    noDetail,
	"DELETE /api/providers/{id}":   noDetail,
	"POST /api/deployments":        keep(deploymentAuditFields...),
	"PATCH /api/deployments/{id}":  keep(with(deploymentAuditFields, "enabled")...),
	"DELETE /api/deployments/{id}": noDetail,
	"PUT /api/prices/{id...}":      keep("input_per_1m", "output_per_1m", "cached_input_per_1m"),
	"POST /api/prices/sync":        noDetail,

	"PUT /api/settings/sso":       noDetail,
	"PUT /api/settings/telemetry": keep("mode", "capture_content"),
	"PUT /api/settings/workspaces": keep("enabled", "runtime", "storage_class", "disk_gb", "memory_mb", "cpus",
		"budget_usd", "max_per_user"),

	"POST /api/workspace-templates":            keep("name", "image"),
	"PATCH /api/workspace-templates/{id}":      keep("name", "image"),
	"DELETE /api/workspace-templates/{id}":     noDetail,
	"POST /api/workspace-templates/{id}/build": noDetail,
	"PUT /api/workspaces/files":                noDetail, // multipart: the body is never read
	"DELETE /api/workspaces/files":             fieldNames,
	"POST /api/workspaces":                     keep("name", "template_id", "image"),
	"DELETE /api/workspaces/{id}":              noDetail,
	"POST /api/workspaces/{id}/start":          noDetail,
	"POST /api/workspaces/{id}/stop":           noDetail,
	"POST /api/workspaces/{id}/upload":         noDetail, // multipart

	"POST /api/gateway-groups":                   keep("name", "region", "allowed_teams", "data_rule"),
	"PATCH /api/gateway-groups/{id}":             keep("name", "region", "allowed_teams", "data_rule"),
	"POST /api/gateway-groups/{id}/rotate-token": noDetail,
	"DELETE /api/gateway-groups/{id}":            noDetail,
	"DELETE /api/gateway-instances/{id}":         noDetail,

	"POST /api/token-issuers":        keep("type", "audience", "claim_mapping"),
	"PATCH /api/token-issuers/{id}":  keep("audience", "claim_mapping", "enabled"),
	"DELETE /api/token-issuers/{id}": noDetail,

	"POST /api/telemetry/destinations":        noDetail,
	"PUT /api/telemetry/destinations/{id}":    noDetail,
	"PATCH /api/telemetry/destinations/{id}":  noDetail,
	"DELETE /api/telemetry/destinations/{id}": noDetail,
	"POST /api/telemetry/destinations/test":   noDetail,
}

// auditSkip are routes that are not administration: a user's own chats and
// playground calls, and the IDE, a whole application behind one prefix.
var auditSkip = map[string]bool{
	"POST /api/chat/completions": true,
	"POST /api/chats":            true,
	"POST /api/lookup":           true, // a read: POST only so the ids fit in the body
	"PUT /api/chats/{id}":        true,
	"DELETE /api/chats/{id}":     true,
	"/api/workspaces/{id}/ide/":  true,
}

// maxDetailBytes bounds the stored detail; past it only field names are kept.
const maxDetailBytes = 4 << 10

func (m *Server) auditing(r *http.Request) bool {
	return m.Audit != nil && r.Method != http.MethodGet && r.Method != http.MethodHead &&
		strings.HasPrefix(r.URL.Path, "/api/") && !auditSkip[r.Pattern]
}

type auditKeyType int

const auditKey auditKeyType = 0

type auditTargetRef struct{ typ, id string }

// auditTarget names the record a request acted on, when the path does not:
// a create's new id, say.
func auditTarget(r *http.Request, typ, id string) {
	if t, ok := r.Context().Value(auditKey).(*auditTargetRef); ok {
		t.typ, t.id = typ, id
	}
}

// audited records the request after next has answered it. It runs once the
// caller is known and before any role check, so a refusal is recorded too.
func (m *Server) audited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !m.auditing(r) {
			next(w, r)
			return
		}
		c := CallerFrom(r.Context())
		detail := requestDetail(r)
		t := &auditTargetRef{}
		t.typ, t.id = pathTarget(r, c)
		aw := &auditWriter{ResponseWriter: w}
		next(aw, r.WithContext(context.WithValue(r.Context(), auditKey, t)))
		m.record(r, c, aw.code(), t.typ, t.id, detail)
	}
}

// recordUnauthenticated records a refused attempt on an audited route.
func (m *Server) recordUnauthenticated(r *http.Request, status int) {
	if !m.auditing(r) {
		return
	}
	typ, id := pathTarget(r, nil)
	m.record(r, nil, status, typ, id, nil)
}

func (m *Server) record(r *http.Request, c *Caller, status int, typ, id string, detail []byte) {
	e := audit.Event{ActorKind: audit.Anonymous, Action: r.Pattern, TargetType: typ, TargetID: id,
		Status: status, ClientIP: console.ClientIP(r, m.TrustedProxies), Detail: detail}
	switch {
	case c == nil:
	case c.UserID == "":
		e.ActorKind = audit.AdminToken
	default:
		e.ActorKind, e.ActorUserID, e.ActorEmail = audit.User, c.UserID, c.Email
	}
	m.Audit.Record(r.Context(), e)
}

var auditResources = map[string]string{
	"users": "user", "teams": "team", "keys": "key", "providers": "provider", "deployments": "deployment",
	"gateway-groups": "gateway_group", "gateway-instances": "gateway_instance", "token-issuers": "token_issuer",
	"workspace-templates": "workspace_template", "workspaces": "workspace", "prices": "price",
}

// pathTarget derives the target from the route: its resource segment and {id}.
func pathTarget(r *http.Request, c *Caller) (typ, id string) {
	path := r.Pattern
	if _, p, ok := strings.Cut(path, " "); ok {
		path = p
	}
	segs := strings.Split(strings.TrimPrefix(path, "/api/"), "/")
	if strings.Contains(path, "{id") {
		id = r.PathValue("id")
	}
	own := ""
	if c != nil {
		own = c.UserID
	}
	switch {
	case segs[0] == "settings" && len(segs) > 1:
		return "setting", segs[1]
	case segs[0] == "telemetry" && len(segs) > 1 && segs[1] == "destinations":
		return "telemetry_destination", id
	case segs[0] == "teams" && strings.Contains(path, "{uid}"):
		return "team_member", id + "/" + r.PathValue("uid")
	case segs[0] == "me":
		return "user", own
	case segs[0] == "workspaces" && len(segs) > 1 && segs[1] == "files":
		return "workspace_files", own
	}
	return auditResources[segs[0]], id
}

// requestDetail is what the route's policy keeps of a JSON body. The body is
// read up to the size handlers accept and handed back to them unchanged.
func requestDetail(r *http.Request) []byte {
	pol, ok := auditPolicy[r.Pattern]
	if !ok {
		pol = fieldNames
	}
	if pol.none || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
	if err != nil {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(buf, &obj) != nil || len(obj) == 0 {
		return nil
	}
	if pol.fields == nil {
		names := sortedKeys(obj)
		b, _ := json.Marshal(map[string]any{"fields": names})
		if len(b) > maxDetailBytes {
			b, _ = json.Marshal(map[string]any{"truncated": true, "field_count": len(names)})
		}
		return b
	}
	kept := map[string]json.RawMessage{}
	for _, f := range pol.fields {
		if v, ok := obj[f]; ok {
			kept[f] = v
		}
	}
	if len(kept) == 0 {
		return nil
	}
	b, _ := json.Marshal(kept)
	if len(b) > maxDetailBytes {
		b, _ = json.Marshal(map[string]any{"truncated": true, "fields": sortedKeys(kept)})
	}
	return b
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// auditWriter keeps the status only. It passes Flush and Hijack through so
// streamed and upgraded responses work as they do unwrapped.
type auditWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *auditWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *auditWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	w.status = http.StatusSwitchingProtocols
	return h.Hijack()
}

func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *auditWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// listAudit pages through the audit log newest first: ?before=<id>&limit=.
func (m *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where, args := []string{"TRUE"}, []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "$?", "$"+strconv.Itoa(len(args))))
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			httpError(w, http.StatusBadRequest, "limit must be 1 to 200")
			return
		}
		limit = n
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			httpError(w, http.StatusBadRequest, "before must be an event id")
			return
		}
		add("id < $?", n)
	}
	if v := q.Get("actor"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpError(w, http.StatusBadRequest, "actor must be a user id")
			return
		}
		add("actor_user_id = $?", id)
	}
	for _, col := range []string{"action", "target_type", "target_id"} {
		if v := q.Get(col); v != "" {
			add(col+" = $?", v)
		}
	}
	switch q.Get("status") {
	case "":
	case "ok":
		where = append(where, "status < 400")
	case "err":
		where = append(where, "status >= 400")
	default:
		httpError(w, http.StatusBadRequest, "status must be ok or err")
		return
	}
	for _, b := range []struct{ param, cond string }{{"since", "ts >= $?"}, {"until", "ts < $?"}} {
		if v := q.Get(b.param); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				httpError(w, http.StatusBadRequest, b.param+" must be an RFC 3339 time")
				return
			}
			add(b.cond, t)
		}
	}
	args = append(args, limit)
	m.list(w, r, `SELECT id, ts, actor_kind, actor_user_id, actor_email, action, target_type, target_id,
		status, client_ip, detail FROM audit_events WHERE `+strings.Join(where, " AND ")+`
		ORDER BY id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
}
