// Package telemetry exports usage events to observability backends. Langfuse
// is the first; anything implementing usage.Writer can be added behind the
// same settings seam.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// Langfuse batches usage events to the Langfuse ingestion API. Config is read
// live from the snapshot, so console changes apply without a restart.
type Langfuse struct {
	config   func() snapshot.Telemetry
	ch       chan usage.Event
	client   *http.Client
	Recorder *Recorder // optional; records exported events for the live feed

	// Delivery outcome, so the console can distinguish "selected for export"
	// from "actually accepted by Langfuse". Without this, a rejected key looks
	// identical to a working one: events appear in the live feed either way.
	mu        sync.Mutex
	delivered int64
	dropped   int64
	lastOKAt  time.Time
	lastErr   string
	lastErrAt time.Time
}

// Health reports delivery outcome for the console.
type Health struct {
	Delivered int64      `json:"delivered"`
	Dropped   int64      `json:"dropped"`
	LastOKAt  *time.Time `json:"last_ok_at,omitempty"`
	LastErr   string     `json:"last_error,omitempty"`
	LastErrAt *time.Time `json:"last_error_at,omitempty"`
}

func (l *Langfuse) Health() Health {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := Health{Delivered: l.delivered, Dropped: l.dropped, LastErr: l.lastErr}
	if !l.lastOKAt.IsZero() {
		t := l.lastOKAt
		h.LastOKAt = &t
	}
	if !l.lastErrAt.IsZero() {
		t := l.lastErrAt
		h.LastErrAt = &t
	}
	return h
}

func (l *Langfuse) note(n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.dropped += int64(n)
		l.lastErr = err.Error()
		l.lastErrAt = time.Now().UTC()
		return
	}
	l.delivered += int64(n)
	l.lastOKAt = time.Now().UTC()
}

func NewLangfuse(config func() snapshot.Telemetry) *Langfuse {
	return &Langfuse{
		config: config,
		ch:     make(chan usage.Event, 8192),
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Write implements usage.Writer; drops when disabled, when the per-request
// export decision said no, or when the buffer is full. The Evaluate decision is
// computed once in the gateway and carried on the event (e.Export).
func (l *Langfuse) Write(e usage.Event) {
	cfg := l.config()
	if !cfg.Enabled || cfg.Type != "langfuse" || !e.Export {
		return
	}
	if l.Recorder != nil {
		l.Recorder.Add(e)
	}
	select {
	case l.ch <- e:
	default: // ponytail: drop-newest under pressure; telemetry is best-effort
	}
}

func (l *Langfuse) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	batch := make([]usage.Event, 0, 100)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		err := l.send(ctx, batch)
		l.note(len(batch), err)
		if err != nil && ctx.Err() == nil {
			slog.Warn("langfuse export failed; events dropped", "events", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for {
				select {
				case e := <-l.ch:
					batch = append(batch, e)
					continue
				default:
				}
				break
			}
			if len(batch) > 0 {
				if err := l.send(fctx, batch); err != nil {
					l.note(len(batch), err)
					slog.Warn("final langfuse export failed", "err", err)
				}
			}
			cancel()
			return
		case e := <-l.ch:
			batch = append(batch, e)
			if len(batch) >= 100 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (l *Langfuse) send(ctx context.Context, events []usage.Event) error {
	cfg := l.config()
	if !cfg.Enabled || cfg.Host == "" {
		return nil
	}
	type item struct {
		ID        string         `json:"id"`
		Type      string         `json:"type"`
		Timestamp string         `json:"timestamp"`
		Body      map[string]any `json:"body"`
	}
	batch := make([]item, 0, len(events)*2)
	for _, e := range events {
		ts := e.TS.UTC().Format(time.RFC3339Nano)
		end := e.TS.Add(time.Duration(e.LatencyMS) * time.Millisecond).UTC().Format(time.RFC3339Nano)
		meta := map[string]any{
			"api_key_id":    e.APIKeyID,
			"team_id":       e.TeamID,
			"provider_id":   e.ProviderID,
			"deployment_id": e.DeploymentID,
			"status_code":   e.StatusCode,
			"stream":        e.Stream,
			"edge_node_id":  e.EdgeNodeID,
		}
		tags := []string{"llm-router", e.ModelName}
		batch = append(batch, item{
			ID: uuid.NewString(), Type: "trace-create", Timestamp: ts,
			Body: map[string]any{
				"id": e.RequestID, "name": e.ModelName, "timestamp": ts,
				"userId": e.UserID, "tags": tags, "metadata": meta,
			},
		})
		gen := map[string]any{
			"id": "gen-" + e.RequestID, "traceId": e.RequestID, "name": e.ModelName,
			"startTime": ts, "endTime": end, "model": e.ModelName,
			"usage": map[string]any{
				"input": e.PromptTokens, "output": e.CompletionTokens, "unit": "TOKENS",
				"totalCost": e.CostUSD,
			},
			"metadata": meta,
		}
		if e.StatusCode >= 400 {
			gen["level"] = "ERROR"
		}
		if e.CaptureContent {
			if len(e.Input) > 0 {
				gen["input"] = json.RawMessage(e.Input)
			}
			if e.Output != "" {
				gen["output"] = e.Output
			}
		}
		batch = append(batch, item{ID: uuid.NewString(), Type: "generation-create", Timestamp: ts, Body: gen})
	}

	payload, err := json.Marshal(map[string]any{"batch": batch})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(cfg.Host, "/")+"/api/public/ingestion", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("langfuse ingestion returned %d", resp.StatusCode)
	}
	return nil
}
