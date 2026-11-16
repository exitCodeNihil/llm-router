package gwsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// A gateway that stays ahead of the database is told to retry at first (the
// control plane may only be lagging); past rewindAfter the database is taken
// as restored in place and gets a new epoch, once, so the gateway's version
// no longer counts against it.
func TestGatewayAheadOfARewoundDatabase(t *testing.T) {
	url := os.Getenv("LLMR_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LLMR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	epoch := func() string {
		var id string
		if err := st.Pool.QueryRow(ctx, `SELECT value #>> '{}' FROM settings WHERE key='config_epoch'`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := epoch()
	if first == "" {
		t.Fatal("no config epoch after migrating")
	}
	snap := &snapshot.Snapshot{Version: 5, EpochID: first, Epoch: "pg." + first}
	s := &Server{Store: st}
	defer func(d time.Duration) { rewindAfter = d }(rewindAfter)
	rewindAfter = 20 * time.Millisecond
	ahead := func(instance string, version int64, ep string) (bool, int) {
		w := httptest.NewRecorder()
		return s.behind(w, snap, instance, ep, version), w.Code
	}
	if b, _ := ahead("gw1", 5, snap.Epoch); b {
		t.Fatal("a gateway on the same version is not ahead")
	}
	if b, _ := ahead("gw1", 9, "another."+first); b {
		t.Fatal("another epoch's version does not count")
	}
	if b, code := ahead("gw1", 9, snap.Epoch); !b || code != http.StatusServiceUnavailable {
		t.Fatalf("ahead: %v %d", b, code)
	}
	if epoch() != first {
		t.Fatal("a new epoch at the first sign of a gateway ahead")
	}
	time.Sleep(40 * time.Millisecond)
	ahead("gw1", 9, snap.Epoch)
	second := epoch()
	if second == first {
		t.Fatal("no new epoch for a gateway that stayed ahead")
	}
	// Another control plane, still on the old snapshot, notices the same
	// rewind: it must not replace the epoch again.
	other := &Server{Store: st}
	w := httptest.NewRecorder()
	other.behind(w, snap, "gw2", snap.Epoch, 9)
	time.Sleep(40 * time.Millisecond)
	other.behind(w, snap, "gw2", snap.Epoch, 9)
	if epoch() != second {
		t.Fatal("the same rewind started two epochs")
	}
	// The next snapshot carries the new epoch: the gateway is no longer ahead.
	b := &snapshot.Builder{Pool: st.Pool}
	built, err := b.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if built.EpochID != second || built.Epoch == "" || built.Epoch == snap.Epoch {
		t.Fatalf("snapshot epoch %q", built.Epoch)
	}
	if b, _ := ahead("gw1", 9, snap.Epoch); !b {
		t.Fatal("still measured against the old snapshot here")
	}
	w = httptest.NewRecorder()
	if s.behind(w, built, "gw1", snap.Epoch, 9) {
		t.Fatal("a gateway from the old epoch is still held back")
	}
}
