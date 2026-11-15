// Package cluster lets several control planes run at once. Each registers in
// Postgres every second, hands the others the request and token counts it took
// in, and reads the committed spend of the scopes any of them wrote to, so
// rate limits and budgets hold across all of them to within a second or two:
// the bound gateways already have.
package cluster

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/limits"
)

const (
	interval = time.Second
	// A replica not seen for peerTTL is gone: its counts stop being awaited
	// and it no longer holds up a key rotation.
	peerTTL = 10 * time.Second
	// Counts older than this no longer fall in a rate window.
	logWindow = 2 * time.Minute
	logKeep   = 3 * time.Minute
	// Rows held back while Postgres can't be written, at most.
	maxUnsent = 50000
)

// Peer is another control plane, as it last registered.
type Peer struct {
	ID      string
	Version string
	KeyFP   string
	State   json.RawMessage
}

// Node is this control plane's part in the cluster.
type Node struct {
	Pool   *pgxpool.Pool
	Limits *limits.Checker
	// Spend reads the committed spend of scopes from Postgres, and when it
	// started reading.
	Spend func(ctx context.Context, scopes []string) (map[string]float64, time.Time, error)
	// State is published with each registration, for the other replicas'
	// consoles; nil publishes nothing.
	State   func() any
	Version string
	KeyFP   string

	seq     int64
	cursors map[string]int64 // last row read, by the replica that wrote it
	unsent  []row
	reread  map[string]bool // scopes whose spend read failed
	ticks   int
	peers   atomic.Pointer[[]Peer]
	failing bool

	// Errors counts ticks that failed, for /metrics.
	Errors atomic.Int64
	once   sync.Once
}

type row struct {
	scope            string
	minute           int64
	requests, tokens int64
	spend            bool
}

// ID names this replica.
func (n *Node) ID() string { return n.Limits.ID() }

// Peers are the other control planes seen in the last few seconds.
func (n *Node) Peers() []Peer {
	if p := n.peers.Load(); p != nil {
		return *p
	}
	return nil
}

// Run registers and shares until ctx ends, then removes the registration.
func (n *Node) Run(ctx context.Context) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n.Tick(ctx)
		select {
		case <-ctx.Done():
			bye, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			n.Pool.Exec(bye, `DELETE FROM cp_replicas WHERE id = $1`, n.ID())
			cancel()
			return
		case <-t.C:
		}
	}
}

// Tick is one round: register, hand over, take in, re-read spend.
func (n *Node) Tick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := n.tick(ctx)
	if err != nil {
		n.Errors.Add(1)
		if !n.failing && ctx.Err() == nil {
			slog.Warn("sharing limits between control planes failed; each enforces its own until it works again", "err", err)
		}
	} else if n.failing {
		slog.Info("sharing limits between control planes works again")
	}
	n.failing = err != nil
}

func (n *Node) tick(ctx context.Context) error {
	n.once.Do(func() {
		n.cursors = map[string]int64{}
		n.reread = map[string]bool{}
	})
	if err := n.register(ctx); err != nil {
		return err
	}
	peers := n.Peers()
	sharing := len(peers) > 0

	// Hand over: this second's counts, and the scopes spend was committed for.
	baseline := n.Limits.Share(sharing)
	rates, touched := n.Limits.Drain()
	if sharing {
		for _, r := range append(baseline, rates...) {
			n.unsent = append(n.unsent, row{scope: r.Scope, minute: r.Minute, requests: r.Requests, tokens: r.Tokens})
		}
		for scope := range touched {
			n.unsent = append(n.unsent, row{scope: scope, spend: true})
		}
		if len(n.unsent) > maxUnsent {
			n.unsent = n.unsent[len(n.unsent)-maxUnsent:]
		}
	} else {
		n.unsent = nil
	}
	var firstErr error
	if len(n.unsent) > 0 {
		if err := n.append(ctx, n.unsent); err != nil {
			firstErr = err
		} else {
			n.unsent = n.unsent[:0]
		}
	}

	// Take in what the others wrote.
	theirs, err := n.read(ctx, peers)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	for scope := range theirs {
		n.reread[scope] = true
	}

	// Read again the spend of every scope written to, here or there.
	for scope := range touched {
		n.reread[scope] = true
	}
	if len(n.reread) > 0 && n.Spend != nil {
		scopes := make([]string, 0, len(n.reread))
		for scope := range n.reread {
			scopes = append(scopes, scope)
		}
		scopes = n.Limits.Budgeted(scopes)
		totals, at, err := n.Spend(ctx, scopes)
		if err != nil {
			// Still committed here: say so again next time.
			for scope, usd := range touched {
				n.Limits.Committed(scope, usd)
			}
			if firstErr == nil {
				firstErr = err
			}
		} else {
			n.Limits.Rebase(at, scopes, totals, touched)
			clear(n.reread)
		}
	}

	if n.ticks++; n.ticks%10 == 0 {
		n.Pool.Exec(ctx, `DELETE FROM cp_rate_log WHERE ts < now() - $1::interval`, logKeep.String())
		n.Pool.Exec(ctx, `DELETE FROM cp_replicas WHERE last_seen < now() - interval '1 hour'`)
	}
	return firstErr
}

// register writes this replica's row and reads the others'.
func (n *Node) register(ctx context.Context) error {
	state := []byte(`{}`)
	if n.State != nil {
		if b, err := json.Marshal(n.State()); err == nil {
			state = b
		}
	}
	if _, err := n.Pool.Exec(ctx, `
		INSERT INTO cp_replicas (id, version, key_fp, state) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET last_seen = now(), version = EXCLUDED.version,
			key_fp = EXCLUDED.key_fp, state = EXCLUDED.state`,
		n.ID(), n.Version, n.KeyFP, state); err != nil {
		return err
	}
	rows, err := n.Pool.Query(ctx, `
		SELECT id, version, key_fp, state FROM cp_replicas
		WHERE id <> $1 AND last_seen > now() - $2::interval ORDER BY id`, n.ID(), peerTTL.String())
	if err != nil {
		return err
	}
	peers, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Peer, error) {
		var p Peer
		return p, r.Scan(&p.ID, &p.Version, &p.KeyFP, &p.State)
	})
	if err != nil {
		return err
	}
	n.peers.Store(&peers)
	for id := range n.cursors {
		gone := true
		for _, p := range peers {
			gone = gone && p.ID != id
		}
		if gone {
			delete(n.cursors, id)
		}
	}
	return nil
}

// append writes rows in one statement, numbered in order. One statement, so
// a reader sees all of them or none, and never a later number without the
// earlier ones.
func (n *Node) append(ctx context.Context, rows []row) error {
	m := len(rows)
	seqs, scopes := make([]int64, m), make([]string, m)
	minutes, requests, tokens := make([]int64, m), make([]int64, m), make([]int64, m)
	spend := make([]bool, m)
	for i, r := range rows {
		seqs[i], scopes[i] = n.seq+int64(i)+1, r.scope
		minutes[i], requests[i], tokens[i], spend[i] = r.minute, r.requests, r.tokens, r.spend
	}
	_, err := n.Pool.Exec(ctx, `
		INSERT INTO cp_rate_log (replica, seq, scope, minute, requests, tokens, spend)
		SELECT $1, * FROM unnest($2::bigint[], $3::text[], $4::bigint[], $5::bigint[], $6::bigint[], $7::bool[])`,
		n.ID(), seqs, scopes, minutes, requests, tokens, spend)
	if err == nil {
		n.seq += int64(m)
	}
	return err
}

// read takes in the rows peers wrote since the last read, and returns the
// scopes they committed spend for. A peer seen for the first time is read
// from two minutes back: what still counts in a rate window.
func (n *Node) read(ctx context.Context, peers []Peer) (map[string]bool, error) {
	if len(peers) == 0 {
		return nil, nil
	}
	ids, after := make([]string, len(peers)), make([]int64, len(peers))
	for i, p := range peers {
		ids[i], after[i] = p.ID, n.cursors[p.ID]
	}
	rows, err := n.Pool.Query(ctx, `
		SELECT l.replica, l.seq, l.scope, l.minute, l.requests, l.tokens, l.spend
		FROM unnest($1::text[], $2::bigint[]) AS c(replica, seq)
		JOIN cp_rate_log l ON l.replica = c.replica AND l.seq > c.seq
		WHERE l.ts > now() - $3::interval
		ORDER BY l.replica, l.seq`, ids, after, logWindow.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	spend := map[string]bool{}
	for rows.Next() {
		var replica string
		var seq int64
		var r limits.Rate
		var isSpend bool
		if err := rows.Scan(&replica, &seq, &r.Scope, &r.Minute, &r.Requests, &r.Tokens, &isSpend); err != nil {
			return spend, err
		}
		n.cursors[replica] = seq
		if isSpend {
			spend[r.Scope] = true
		} else {
			n.Limits.IngestPeer(r)
		}
	}
	return spend, rows.Err()
}
