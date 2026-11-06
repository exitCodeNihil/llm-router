package metrics

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Routes the control plane's request counter is split by: the Rust
// gateway's, so the fleet's counters add up, and then the control plane's own.
var httpRoutes = [...]string{"chat_completions", "completions", "embeddings", "messages", "models", "count_tokens",
	"other", "api", "gw_sync"}

const (
	routeOther = 6
	routeAPI   = 7
	routeSync  = 8
)

var statusClasses = [...]string{"1xx", "2xx", "3xx", "4xx", "5xx"}

// HTTP instruments the control plane's listener: connections through
// http.Server.ConnState, requests through Middleware.
type HTTP struct {
	open     atomic.Int64
	accepted atomic.Uint64
	inFlight atomic.Int64
	requests [len(httpRoutes)][len(statusClasses)]atomic.Uint64
	latency  *Histogram
}

func NewHTTP() *HTTP { return &HTTP{latency: NewHistogram(LatencyBuckets)} }

// ConnState is an http.Server.ConnState hook.
func (h *HTTP) ConnState(_ net.Conn, s http.ConnState) {
	switch s {
	case http.StateNew:
		h.accepted.Add(1)
		h.open.Add(1)
	case http.StateClosed, http.StateHijacked:
		h.open.Add(-1)
	}
}

// OpenConnections is the client connections open now.
func (h *HTTP) OpenConnections() int64 { return h.open.Load() }

// InFlight is the /v1 requests being served now.
func (h *HTTP) InFlight() int64 { return h.inFlight.Load() }

// route classifies a request with a fixed set of names, so a scanner trying
// paths cannot add series.
func route(r *http.Request) int {
	p := r.URL.Path
	switch {
	case p == "/v1/chat/completions":
		return 0
	case p == "/v1/completions":
		return 1
	case p == "/v1/embeddings":
		return 2
	case p == "/v1/messages":
		return 3
	case p == "/v1/models" || strings.HasPrefix(p, "/v1/models/"):
		return 4
	case p == "/v1/messages/count_tokens":
		return 5
	case strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/auth/"):
		return routeAPI
	case strings.HasPrefix(p, "/gw/"):
		return routeSync
	}
	return routeOther
}

// Middleware counts requests by route and status class. Requests to /v1 also
// count in flight and in the latency histogram, which so cover the same
// traffic as the Rust gateway's: the console's API and the gateways'
// long-polls would only blur them.
func (h *HTTP) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt := route(r)
		data := rt < routeOther || strings.HasPrefix(r.URL.Path, "/v1/")
		start := time.Now()
		if data {
			h.inFlight.Add(1)
		}
		sw := &statusRecorder{ResponseWriter: w}
		defer func() {
			p := recover()
			if data {
				h.inFlight.Add(-1)
				h.latency.Observe(time.Since(start).Seconds())
			}
			status := sw.status
			switch {
			case p != nil:
				status = http.StatusInternalServerError
			case status == 0:
				status = http.StatusOK
			}
			h.requests[rt][min(max(status/100, 1), 5)-1].Add(1)
			if p != nil {
				panic(p)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// Collect writes the listener's families.
func (h *HTTP) Collect(w *PromWriter) {
	w.Gauge("llmr_open_connections", "Client connections open", float64(h.open.Load()))
	w.Counter("llmr_connections_total", "Client connections accepted", float64(h.accepted.Load()))
	w.Gauge("llmr_in_flight", "Requests to /v1 being served", float64(h.inFlight.Load()))
	w.Family("llmr_requests_total", "counter", "Requests served")
	for r, rn := range httpRoutes {
		for c, cn := range statusClasses {
			if v := h.requests[r][c].Load(); v > 0 {
				w.Sample("llmr_requests_total", float64(v), "route", rn, "code", cn)
			}
		}
	}
	w.Histogram("llmr_request_duration_seconds", "Latency of requests to /v1", h.latency)
}

// statusRecorder keeps the status a handler wrote, and passes the optional
// interfaces of the ResponseWriter it wraps on: streaming needs Flush, the
// workspace IDE proxy needs Hijack.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 && code >= 200 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		if s.status == 0 {
			s.status = http.StatusSwitchingProtocols
		}
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
