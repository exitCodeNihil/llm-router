package usage

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/store"
)

// zoneDB is a new, migrated database beside the test database, with zone as
// its default time zone. Without CREATEDB the test is skipped.
func zoneDB(t *testing.T, name, zone string) *store.Store {
	t.Helper()
	base := testStore(t)
	ctx := context.Background()
	var db string
	if err := base.Pool.QueryRow(ctx, `SELECT current_database() || '_' || $1`, name).Scan(&db); err != nil {
		t.Fatal(err)
	}
	drop := func() { base.Pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+db+` WITH (FORCE)`) }
	drop()
	if _, err := base.Pool.Exec(ctx, `CREATE DATABASE `+db); err != nil {
		t.Skipf("cannot create a database: %v", err)
	}
	t.Cleanup(drop)
	if _, err := base.Pool.Exec(ctx, `ALTER DATABASE `+db+` SET timezone = '`+zone+`'`); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("LLMR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

// legacy031 makes the partitions 0.3.1 made for the window around now: bounds
// as dates in the database's time zone, and five indexes on each.
func legacy031(t *testing.T, st *store.Store, now time.Time) {
	t.Helper()
	ctx := context.Background()
	lo, hi := Window(now)
	for m := lo; m.Before(hi); m = m.AddDate(0, 1, 0) {
		p := "usage_events_" + m.Format("2006_01")
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF usage_events FOR VALUES FROM ('%s') TO ('%s')`,
			p, m.Format("2006-01-02"), m.AddDate(0, 1, 0).Format("2006-01-02"))); err != nil {
			t.Fatal(err)
		}
		for _, idx := range [][2]string{{"_team_ts", "(team_id, ts)"}, {"_key_ts", "(api_key_id, ts)"},
			{"_user_ts", "(user_id, ts)"}, {"_ts", "(ts DESC)"}, {"_request", "(request_id)"}} {
			if _, err := st.Pool.Exec(ctx, `CREATE INDEX `+p+idx[0]+` ON `+p+` `+idx[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// checkIndexes asserts the adopted layout: primary key (ts, id), the
// partitioned table's five indexes valid, and each partition with exactly one
// index attached to each and no other. It returns every index's oid.
func checkIndexes(t *testing.T, st *store.Store) []uint32 {
	t.Helper()
	ctx := context.Background()
	var cols string
	if err := st.Pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'usage_events'::regclass AND contype = 'p'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != "PRIMARY KEY (ts, id)" {
		t.Errorf("usage_events key is %s", cols)
	}
	want := []string{"usage_events_key_ts", "usage_events_pkey", "usage_events_request", "usage_events_team_ts", "usage_events_user_ts"}
	rows, _ := st.Pool.Query(ctx, `SELECT c.relname || CASE WHEN i.indisvalid THEN '' ELSE ' (invalid)' END
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = 'usage_events'::regclass ORDER BY 1`)
	if got, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || !slices.Equal(got, want) {
		t.Errorf("usage_events indexes %v %v, want %v", got, err, want)
	}
	rows, _ = st.Pool.Query(ctx, `
		SELECT t.relname, x.oid, coalesce(p.relname, '-'), x.relname
		FROM pg_inherits h JOIN pg_class t ON t.oid = h.inhrelid
		JOIN pg_index i ON i.indrelid = t.oid JOIN pg_class x ON x.oid = i.indexrelid
		LEFT JOIN pg_inherits hp ON hp.inhrelid = i.indexrelid LEFT JOIN pg_class p ON p.oid = hp.inhparent
		WHERE h.inhparent = 'usage_events'::regclass ORDER BY 1, 3`)
	parents := map[string][]string{}
	var oids []uint32
	var part, parent, index string
	var oid uint32
	if _, err := pgx.ForEachRow(rows, []any{&part, &oid, &parent, &index}, func() error {
		parents[part] = append(parents[part], parent)
		oids = append(oids, oid)
		if strings.HasSuffix(index, "_ts") && parent == "-" {
			t.Errorf("%s still has the (ts DESC) index", part)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(parents) == 0 {
		t.Fatal("no partitions")
	}
	for part, got := range parents {
		if !slices.Equal(got, want) {
			t.Errorf("%s: indexes attached to %v, want one each of %v", part, got, want)
		}
	}
	slices.Sort(oids)
	return oids
}

// A database laid out the 0.3.1 way, with rows, ends up with the key on
// (ts, id) and every index on the partitioned table, built once; a second
// pass changes nothing, and a later partition gets everything with it.
func TestIndexAdoption(t *testing.T) {
	st := zoneDB(t, "adopt", "UTC")
	ctx := context.Background()
	w := NewPGWriter(st.Pool)
	now := time.Now().UTC()
	legacy031(t, st, now)
	lo, hi := Window(now)
	if _, err := st.Pool.Exec(ctx, `INSERT INTO usage_events (ts, request_id, model_name, team_id)
		SELECT $1::timestamptz + (g * ($2::timestamptz - $1::timestamptz) / 30000), 'r' || g, 'm', gen_random_uuid()
		FROM generate_series(0, 29999) g`, lo, hi.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// An earlier pass that stopped after one index of the partitioned table,
	// still invalid, and a partition created after it: that partition got its
	// copy of the index at creation and must not get a second.
	if _, err := st.Pool.Exec(ctx, `CREATE INDEX usage_events_team_ts ON ONLY usage_events (team_id, ts)`); err != nil {
		t.Fatal(err)
	}
	if err := w.ensurePartitions(ctx, now.AddDate(0, 1, 0)); err != nil {
		t.Fatal(err)
	}

	if err := w.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	first := checkIndexes(t, st)
	if err := w.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	if again := checkIndexes(t, st); !slices.Equal(first, again) {
		t.Errorf("a second pass changed the indexes: %v -> %v", first, again)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_events`).Scan(&n); err != nil || n != 30000 {
		t.Fatalf("%d rows (%v), want 30000", n, err)
	}

	if err := w.ensurePartitions(ctx, now.AddDate(0, 2, 0)); err != nil {
		t.Fatal(err)
	}
	checkIndexes(t, st)

	// The Requests feed, newest first with a keyset cursor, reads the key
	// backwards: no sort.
	if _, err := st.Pool.Exec(ctx, `ANALYZE usage_events`); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Pool.Query(ctx, `EXPLAIN SELECT e.id, e.ts, e.request_id FROM usage_events e
		WHERE e.ts >= $1 AND (e.ts, e.id) < ($2, $3) ORDER BY e.ts DESC, e.id DESC LIMIT 50`,
		lo, hi.Add(-48*time.Hour), int64(1<<40))
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	t.Logf("request feed plan:\n%s", plan)
	if !strings.Contains(plan, "Index Scan Backward using usage_events_") || !strings.Contains(plan, "_pkey") || strings.Contains(plan, "Sort") {
		t.Errorf("the feed does not read the (ts, id) key backwards:\n%s", plan)
	}
}

// A new database ends up as an adopted one.
func TestIndexesOnNewDatabase(t *testing.T) {
	st := zoneDB(t, "fresh", "UTC")
	w := NewPGWriter(st.Pool)
	if err := w.EnsurePartitions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.EnsureIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkIndexes(t, st)
}

// Partitions 0.3.1 made with bounds in a non-UTC database's zone are followed
// by ones that close the gap or overlap, then by UTC months, with no instant
// left without a partition or in two. On UTC nothing changes. Retention then
// keeps every partition that holds rows inside what it keeps.
func TestPartitionBoundsConverge(t *testing.T) {
	for _, zone := range []string{"America/New_York", "Europe/Berlin", "Asia/Tokyo", "UTC"} {
		t.Run(zone, func(t *testing.T) {
			name := strings.ToLower(zone[strings.LastIndex(zone, "/")+1:])
			st := zoneDB(t, "tz_"+name, zone)
			ctx := context.Background()
			w := NewPGWriter(st.Pool)
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			utc := func(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }
			start := time.Date(2026, 12, 15, 12, 0, 0, 0, time.UTC)
			legacy031(t, st, start) // 2026-11 to 2027-01
			for step := range 3 {
				if err := w.ensurePartitions(ctx, start.AddDate(0, step, 0)); err != nil {
					t.Fatal(err)
				}
			}
			spans := readSpans(t, st)
			if len(spans) != 5 {
				t.Fatalf("%d partitions, want 5: %v", len(spans), spans)
			}
			if zone != "UTC" && spans[0].from.Equal(utc(2026, 11)) {
				t.Fatal("the 0.3.1 partitions were not made in the database's zone")
			}
			for i := 1; i < len(spans); i++ {
				if !spans[i-1].to.Equal(spans[i].from) {
					t.Errorf("%s ends %v, %s starts %v", spans[i-1].name, spans[i-1].to, spans[i].name, spans[i].from)
				}
			}
			// The first new partition ends on the UTC month, the next is one.
			if feb, mar := spans[3], spans[4]; !feb.to.Equal(utc(2027, 3)) || !mar.from.Equal(utc(2027, 3)) || !mar.to.Equal(utc(2027, 4)) {
				t.Errorf("new partitions %v, %v", feb, mar)
			}
			if zone == "UTC" {
				for i, s := range spans {
					if m := utc(2026, 11).AddDate(0, i, 0); !s.from.Equal(m) || !s.to.Equal(m.AddDate(0, 1, 0)) {
						t.Errorf("%s is [%v, %v), want the UTC month", s.name, s.from, s.to)
					}
				}
			}
			// Each month's first instant, in UTC and in the database's zone,
			// and the instant before it.
			for m := utc(2026, 12); m.Before(utc(2027, 4)); m = m.AddDate(0, 1, 0) {
				local := time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, loc)
				for _, ts := range []time.Time{m, m.Add(-time.Microsecond), local, local.Add(-time.Microsecond)} {
					if _, err := st.Pool.Exec(ctx, `INSERT INTO usage_events (ts, request_id, model_name) VALUES ($1, 'edge', 'm')`, ts); err != nil {
						t.Errorf("insert at %v: %v", ts, err)
					}
				}
			}

			kept := func(since time.Time) (n int) {
				st.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE ts >= $1`, since).Scan(&n)
				return n
			}
			before := utc(2027, 2)
			want := kept(before)
			w.Retention = Retention{RawMonths: 2}
			if err := w.Prune(ctx, time.Date(2027, 4, 15, 0, 0, 0, 0, time.UTC)); err != nil {
				t.Fatal(err)
			}
			if got := kept(before); got != want {
				t.Errorf("retention dropped rows it keeps: %d left of %d", got, want)
			}
			for _, s := range readSpans(t, st) {
				if !s.to.After(before) {
					t.Errorf("%s [%v, %v) is past the retention and still there", s.name, s.from, s.to)
				}
			}
			if w.PartitionsDropped.Load() == 0 {
				t.Error("nothing dropped")
			}
		})
	}
}

type namedSpan struct {
	name string
	span
}

func readSpans(t *testing.T, st *store.Store) []namedSpan {
	t.Helper()
	conn, err := st.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	have, err := partitionSpans(context.Background(), conn.Conn(), "usage_events")
	if err != nil {
		t.Fatal(err)
	}
	var out []namedSpan
	for name, s := range have {
		out = append(out, namedSpan{name, s})
	}
	slices.SortFunc(out, func(a, b namedSpan) int { return a.from.Compare(b.from) })
	return out
}
