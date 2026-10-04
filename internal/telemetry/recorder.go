package telemetry

import (
	"sync"
	"time"

	"github.com/exitcodenihil/llm-router/internal/usage"
)

const recorderRingSize = 200

// Exported is one recently exported event (ids resolved to names by the reader).
type Exported struct {
	TS           time.Time
	KeyID        string
	Model        string
	DeploymentID string
	ProviderID   string
	LatencyMS    int
	CostUSD      float64
	RuleID       string
	Capture      bool
}

// Recorder is a small in-memory ring of recently exported events, powering the
// Observability "Exported right now" feed. ponytail: per-process, lost on restart.
type Recorder struct {
	mu   sync.Mutex
	ring []Exported
}

func NewRecorder() *Recorder { return &Recorder{} }

func (r *Recorder) Add(e usage.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ring = append(r.ring, Exported{
		TS: e.TS, KeyID: e.APIKeyID, Model: e.ModelName, DeploymentID: e.DeploymentID,
		ProviderID: e.ProviderID, LatencyMS: e.LatencyMS, CostUSD: e.CostUSD,
		RuleID: e.RuleID, Capture: e.CaptureContent,
	})
	if len(r.ring) > recorderRingSize {
		r.ring = r.ring[len(r.ring)-recorderRingSize:]
	}
}

// Snapshot returns exported events newest-first.
func (r *Recorder) Snapshot() []Exported {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Exported, len(r.ring))
	for i, e := range r.ring {
		out[len(r.ring)-1-i] = e
	}
	return out
}
