package mgmt

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxPageLimit = 200

type pageParams struct {
	limit    int
	after, q string
}

// readPage reads ?limit, ?after and ?q. paged is false when limit is absent,
// and the caller then answers the full list as before; after and q without a
// limit are refused rather than ignored.
func readPage(w http.ResponseWriter, r *http.Request) (p pageParams, paged, ok bool) {
	qs := r.URL.Query()
	p.after, p.q = qs.Get("after"), strings.TrimSpace(qs.Get("q"))
	if !qs.Has("limit") {
		if p.after != "" || p.q != "" || qs.Has("not_in_team") {
			httpError(w, http.StatusBadRequest, "after, q and not_in_team need limit")
			return p, false, false
		}
		return p, false, true
	}
	n, err := strconv.Atoi(qs.Get("limit"))
	if err != nil || n < 1 || n > maxPageLimit {
		httpError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(maxPageLimit))
		return p, true, false
	}
	p.limit = n
	return p, true, true
}

// pagedList is a keyset-paged query. cols are the sort columns (unique
// together) and casts the Postgres type each cursor value is bound as, so the
// comparison matches the ORDER BY (citext compares case-insensitively).
type pagedList struct {
	sel    string // SELECT … FROM … with its joins, no WHERE
	where  []string
	args   []any
	cols   []string
	casts  []string
	fields []string // the row field holding each col's value
	desc   bool
	search []string // expressions matched by ?q
}

func (l *pagedList) arg(v any) string {
	l.args = append(l.args, v)
	return "$" + strconv.Itoa(len(l.args))
}

func cursorValue(v any) string {
	switch x := v.(type) {
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case string:
		return x
	}
	return ""
}

func (l *pagedList) decodeCursor(after string) ([]string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(after)
	if err != nil {
		return nil, false
	}
	var vals []string
	if json.Unmarshal(raw, &vals) != nil || len(vals) != len(l.cols) {
		return nil, false
	}
	for i, v := range vals {
		switch l.casts[i] {
		case "uuid":
			if _, err := uuid.Parse(v); err != nil {
				return nil, false
			}
		case "timestamptz":
			if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
				return nil, false
			}
		}
	}
	return vals, true
}

func (m *Server) servePage(w http.ResponseWriter, r *http.Request, p pageParams, l pagedList) {
	if p.after != "" {
		vals, ok := l.decodeCursor(p.after)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = l.arg(v) + "::" + l.casts[i]
		}
		op := " > "
		if l.desc {
			op = " < "
		}
		l.where = append(l.where, "("+strings.Join(l.cols, ", ")+")"+op+"("+strings.Join(ph, ", ")+")")
	}
	if p.q != "" && len(l.search) > 0 {
		ph := l.arg(likeEscape(p.q))
		conds := make([]string, len(l.search))
		for i, e := range l.search {
			conds[i] = e + `::text ILIKE '%' || ` + ph + ` || '%'`
		}
		l.where = append(l.where, "("+strings.Join(conds, " OR ")+")")
	}
	order := strings.Join(l.cols, ", ")
	if l.desc {
		order = strings.Join(l.cols, " DESC, ") + " DESC"
	}
	sql := l.sel
	if len(l.where) > 0 {
		sql += " WHERE " + strings.Join(l.where, " AND ")
	}
	sql += " ORDER BY " + order + " LIMIT " + strconv.Itoa(p.limit+1)
	rows, err := listRows(r.Context(), m.reads(), sql, l.args...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := ""
	if len(rows) > p.limit {
		rows = rows[:p.limit]
		last := rows[p.limit-1]
		vals := make([]string, len(l.fields))
		for i, f := range l.fields {
			vals[i] = cursorValue(last[f])
		}
		raw, _ := json.Marshal(vals)
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "next": next})
}

const lookupMax = 100

// lookup names ids the console already holds: a read, carried by POST so the
// ids fit. Admins resolve everything; others only themself and the people
// they share a team with, the keys they could list, and their own teams.
func (m *Server) lookup(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Users []string `json:"users"`
		Keys  []string `json:"keys"`
		Teams []string `json:"teams"`
	}](w, r)
	if !ok {
		return
	}
	parse := func(in []string) ([]uuid.UUID, bool) {
		if len(in) > lookupMax {
			httpError(w, http.StatusBadRequest, "at most "+strconv.Itoa(lookupMax)+" ids per kind")
			return nil, false
		}
		out := make([]uuid.UUID, 0, len(in))
		for _, s := range in {
			id, err := uuid.Parse(s)
			if err != nil {
				httpError(w, http.StatusBadRequest, "invalid id: "+s)
				return nil, false
			}
			out = append(out, id)
		}
		return out, true
	}
	users, ok1 := parse(req.Users)
	if !ok1 {
		return
	}
	keys, ok2 := parse(req.Keys)
	if !ok2 {
		return
	}
	teams, ok3 := parse(req.Teams)
	if !ok3 {
		return
	}
	c := CallerFrom(r.Context())
	var uq, kq, tq string
	uargs, kargs, targs := []any{users}, []any{keys}, []any{teams}
	if c.IsAdmin {
		uq = `SELECT id, email FROM users WHERE id = ANY($1)`
		kq = `SELECT id, name FROM api_keys WHERE id = ANY($1)`
		tq = `SELECT id, name FROM teams WHERE id = ANY($1)`
	} else {
		uargs, kargs, targs = append(uargs, c.UserID), append(kargs, c.UserID), append(targs, c.UserID)
		uq = `SELECT id, email FROM users WHERE id = ANY($1) AND (id = $2 OR id IN
			(SELECT user_id FROM team_members WHERE team_id IN (SELECT team_id FROM team_members WHERE user_id = $2)))`
		kq = `SELECT id, name FROM api_keys WHERE id = ANY($1)
			AND (user_id = $2 OR team_id IN (SELECT team_id FROM team_members WHERE user_id = $2))`
		tq = `SELECT id, name FROM teams WHERE id = ANY($1) AND id IN (SELECT team_id FROM team_members WHERE user_id = $2)`
	}
	out := map[string]map[string]string{}
	for name, q := range map[string]struct {
		sql  string
		args []any
	}{"users": {uq, uargs}, "keys": {kq, kargs}, "teams": {tq, targs}} {
		got := map[string]string{}
		if len(q.args[0].([]uuid.UUID)) > 0 {
			rows, err := m.reads().Query(r.Context(), q.sql, q.args...)
			if err != nil {
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}
			for rows.Next() {
				var id uuid.UUID
				var label string
				if err := rows.Scan(&id, &label); err != nil {
					rows.Close()
					httpError(w, http.StatusInternalServerError, err.Error())
					return
				}
				got[id.String()] = label
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		out[name] = got
	}
	writeJSON(w, http.StatusOK, out)
}
