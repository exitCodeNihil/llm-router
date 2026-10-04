package edge

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/exitcodenihil/llm-router/internal/usage"
)

// Shipper is the edge implementation of usage.Writer: it buffers events in
// memory and POSTs gzip batches to the control plane.
// ponytail: in-memory spool only — events since the last ack are lost if the
// node crashes; add a disk spool file when that matters.
type Shipper struct {
	ControlPlaneURL string
	NodeToken       string

	mu     sync.Mutex
	buf    []usage.Event
	http   *http.Client
}

const (
	shipBatch    = 500
	shipInterval = 5 * time.Second
	shipMaxBuf   = 50000
)

func NewShipper(controlPlaneURL, nodeToken string) *Shipper {
	return &Shipper{
		ControlPlaneURL: controlPlaneURL,
		NodeToken:       nodeToken,
		http:            &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *Shipper) Write(e usage.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) >= shipMaxBuf {
		s.buf = s.buf[1:] // drop oldest
	}
	s.buf = append(s.buf, e)
}

func (s *Shipper) Run(ctx context.Context) {
	ticker := time.NewTicker(shipInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.ship(context.Background()) // final best-effort flush
			return
		case <-ticker.C:
			s.ship(ctx)
		}
	}
}

// ship sends the buffer in batches; failed batches stay buffered for retry.
func (s *Shipper) ship(ctx context.Context) {
	for {
		s.mu.Lock()
		if len(s.buf) == 0 {
			s.mu.Unlock()
			return
		}
		n := min(len(s.buf), shipBatch)
		batch := make([]usage.Event, n)
		copy(batch, s.buf[:n])
		s.mu.Unlock()

		if err := s.post(ctx, batch); err != nil {
			if ctx.Err() == nil {
				slog.Warn("edge: usage ship failed; will retry", "events", n, "err", err)
			}
			return
		}
		s.mu.Lock()
		s.buf = s.buf[n:]
		s.mu.Unlock()
	}
}

func (s *Shipper) post(ctx context.Context, batch []usage.Event) error {
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	if err := json.NewEncoder(gz).Encode(batch); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.ControlPlaneURL+"/edge/v1/usage", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.NodeToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return &shipError{status: resp.StatusCode}
	}
	return nil
}

type shipError struct{ status int }

func (e *shipError) Error() string { return http.StatusText(e.status) }
