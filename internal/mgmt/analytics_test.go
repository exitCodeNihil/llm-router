package mgmt

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every usage query has a lower time bound, however the range is given.
func TestUsageFilterAlwaysBoundsTime(t *testing.T) {
	admin := &Caller{IsAdmin: true}
	now := time.Now().UTC() // UTC: an offset's "+" would need escaping in the query
	for _, tc := range []struct {
		name, query string
		wantErr     bool
		since       time.Time // expected lower bound, within a minute
	}{
		{"default", "", false, now.AddDate(0, 0, -30)},
		{"days", "days=7", false, now.AddDate(0, 0, -7)},
		{"since", "since=" + now.Add(-time.Hour).Format(time.RFC3339), false, now.Add(-time.Hour)},
		{"until alone", "until=" + now.AddDate(0, 0, -10).Format(time.RFC3339) + "&days=2", false, now.AddDate(0, 0, -12)},
		{"bad since", "since=yesterday", true, time.Time{}},
		{"bad until", "until=1700000000", true, time.Time{}},
		{"too long", "since=" + now.AddDate(-2, 0, 0).Format(time.RFC3339), true, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUsageFilter(httptest.NewRequest("GET", "/api/usage/stats?"+tc.query, nil), admin)
			if (f.Err != nil) != tc.wantErr {
				t.Fatalf("Err = %v, want error %v", f.Err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !strings.Contains(f.where(), "e.ts >= $") {
				t.Fatalf("no lower bound in %q", f.where())
			}
			got, ok := f.args[len(f.args)-1].(time.Time)
			if !ok || got.Sub(tc.since).Abs() > time.Minute {
				t.Fatalf("lower bound = %v, want about %v", f.args[len(f.args)-1], tc.since)
			}
		})
	}
}
