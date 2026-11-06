// Package telemetry exports usage events as traces to the configured
// destinations. The capture policy (snapshot.Telemetry) decides what may
// leave; each destination's kind turns events into its backend's requests.
// Adding a backend is one Kind, registered in main.
package telemetry

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// Kind is a destination type.
type Kind interface {
	Name() string  // stored in telemetry_destinations.kind
	Label() string // shown in the console
	Beta() bool
	// SecretFields are the kind's write-only fields, kept encrypted.
	SecretFields() []string
	Validate(*snapshot.Destination) error
	New(*snapshot.Destination) (Sink, error)
}

// Sink delivers batches to one destination.
type Sink interface {
	// Export sends one batch. A failure the queue may retry or split is an
	// *ExportError.
	Export(ctx context.Context, batch []*Record) error
	// TraceURL is a link to a trace with {trace_id} in it; "" = none.
	TraceURL(ctx context.Context) string
}

// ExportError is a failed export, classified for the queue.
type ExportError struct {
	Retryable  bool
	Status     int           // HTTP status; 0 for transport failures
	RetryAfter time.Duration // from the response, 0 = none
	Msg        string
}

func (e *ExportError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg)
	}
	return e.Msg
}

var (
	kindsMu sync.RWMutex
	kinds   = map[string]Kind{}
)

// Register adds kinds. Registering a name twice replaces it.
func Register(ks ...Kind) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	for _, k := range ks {
		kinds[k.Name()] = k
	}
}

// Lookup returns the kind with this name, or nil.
func Lookup(name string) Kind {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	return kinds[name]
}

// Kinds returns the registered kinds by name.
func Kinds() []Kind {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	out := make([]Kind, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
