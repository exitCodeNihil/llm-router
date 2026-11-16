package snapshot

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Only a database that cannot be reached for now fails the build: an epoch
// with its cluster part missing for a while would read to gateways as another
// database. A server that will never answer the query goes without that part.
func TestUnreachable(t *testing.T) {
	for code, want := range map[string]bool{
		"08006": true, "53300": true, "57P01": true, "57014": true,
		"42501": false, "42883": false, "0A000": false, "XX000": false, "55000": false,
	} {
		err := fmt.Errorf("query: %w", &pgconn.PgError{Code: code})
		if got := unreachable(err); got != want {
			t.Errorf("unreachable(%s) = %v, want %v", code, got, want)
		}
	}
	if !unreachable(errors.New("connection reset")) {
		t.Error("a network error is the database out of reach")
	}
}
