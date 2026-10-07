// Command mock is the fake LLM upstream every benchmarked gateway points at. It speaks the
// OpenAI chat completions and Anthropic messages APIs, streamed or not, and takes its timing
// from the requested model name, so all gateways see the same predictable backend.
//
// It also keeps the ground truth the gateways are checked against: requests, tokens and
// connections served, and how long it spent on each request, keyed by the LGID:<n>: tag the load
// generator puts in every prompt. That lets the load generator compute each request's gateway
// overhead exactly instead of subtracting percentiles.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type profile struct {
	ttft    time.Duration // before the first byte, headers included
	chunks  int           // one output token each
	gap     time.Duration // between chunks
	status  int           // always answer with this error
	failPct int           // answer 500 on this share of requests
	hang    bool          // never answer
}

// Upstream names are looked up after the last '/' or ':' and before the first '.', so
// "openai/fast" and "fast.backup" are both the fast profile. The suffix lets one gateway map
// the same profile under several names.
var profiles = map[string]profile{
	"fast": {chunks: 20},
	"chat": {ttft: 300 * time.Millisecond, chunks: 100, gap: 20 * time.Millisecond},
	"long": {ttft: 300 * time.Millisecond, chunks: 10, gap: 3 * time.Second},
	// 50 chunks 10 ms or 0 ms apart: the streaming shapes of an earlier independent study.
	"tok10":  {ttft: 10 * time.Millisecond, chunks: 50, gap: 10 * time.Millisecond},
	"tok0":   {chunks: 50},
	"slow":   {ttft: 30 * time.Second, chunks: 20},
	"flaky":  {chunks: 20, failPct: 10},
	"err500": {status: http.StatusInternalServerError},
	"err429": {status: http.StatusTooManyRequests},
	"hang":   {hang: true},
}

var stats struct {
	requests, inFlight, maxInFlight, completed, cancelled, failed atomic.Int64
	conns, promptTokens, completionTokens, usageRequested         atomic.Int64
	notFound                                                      atomic.Int64
	lastNotFound                                                  atomic.Value // "METHOD /path"
}

// Response ids must be unique for the life of the process, like a real provider's: LiteLLM keys
// its spend log on them and silently drops repeats.
var (
	seq    atomic.Int64
	idBase = strconv.FormatInt(time.Now().UnixNano(), 36)
)

// timing is one request as the mock saw it: from the handler starting to the first and last
// byte written. Durations only, so no clock has to agree with the load generator's.
type timing struct{ id, first, last int64 }

var (
	timingsMu sync.Mutex
	timings   []timing
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	tlsAddr := flag.String("tls-addr", "", "also serve TLS here, with -cert and -key")
	cert := flag.String("cert", "", "TLS certificate (PEM)")
	key := flag.String("key", "", "TLS key (PEM)")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handle(false))
	mux.HandleFunc("POST /v1/messages", handle(true))
	mux.HandleFunc("GET /v1/models", models)
	mux.HandleFunc("GET /stats", statsHandler)
	mux.HandleFunc("GET /timings", timingsHandler)
	mux.HandleFunc("POST /reset", reset)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		stats.notFound.Add(1)
		stats.lastNotFound.Store(r.Method + " " + r.URL.Path)
		http.NotFound(w, r)
	})

	countConns := func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			stats.conns.Add(1)
		}
	}
	if *tlsAddr != "" {
		tls := &http.Server{Addr: *tlsAddr, Handler: mux, ConnState: countConns}
		go func() { log.Fatal(tls.ListenAndServeTLS(*cert, *key)) }()
	}
	log.Printf("mock listening on %s", *addr)
	log.Fatal((&http.Server{Addr: *addr, Handler: mux, ConnState: countConns}).ListenAndServe())
}

type request struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func handle(anthropic bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name := req.Model[strings.LastIndexAny(req.Model, "/:")+1:]
		if i := strings.IndexByte(name, '.'); i >= 0 {
			name = name[:i]
		}
		p, ok := profiles[name]
		if !ok {
			http.Error(w, "unknown model "+req.Model, http.StatusNotFound)
			return
		}

		stats.requests.Add(1)
		n := stats.inFlight.Add(1)
		defer stats.inFlight.Add(-1)
		for m := stats.maxInFlight.Load(); n > m && !stats.maxInFlight.CompareAndSwap(m, n); m = stats.maxInFlight.Load() {
		}
		if req.Stream && req.StreamOptions.IncludeUsage {
			stats.usageRequested.Add(1)
		}

		ctx := r.Context()
		switch {
		case p.hang:
			<-ctx.Done()
			stats.cancelled.Add(1)
			return
		case p.status != 0:
			writeError(w, anthropic, p.status)
			stats.failed.Add(1)
			return
		case p.failPct > 0 && rand.IntN(100) < p.failPct:
			writeError(w, anthropic, http.StatusInternalServerError)
			stats.failed.Add(1)
			return
		}

		prompt := max(len(body)/4, 1)
		id := idBase + strconv.FormatInt(seq.Add(1), 36)
		var first time.Duration
		var done bool
		if req.Stream {
			first, done = stream(ctx, w, anthropic, req, p, id, prompt)
		} else {
			if done = sleep(ctx, p.ttft+time.Duration(p.chunks)*p.gap); done {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, whole(anthropic, req.Model, id, prompt, p.chunks))
				first = time.Since(began)
			}
		}
		if !done {
			stats.cancelled.Add(1)
			return
		}
		stats.completed.Add(1)
		stats.promptTokens.Add(int64(prompt))
		stats.completionTokens.Add(int64(p.chunks))
		if tag, ok := lgid(body); ok {
			timingsMu.Lock()
			timings = append(timings, timing{tag, int64(first), int64(time.Since(began))})
			timingsMu.Unlock()
		}
	}
}

// stream writes the SSE response, flushing every event the way real providers do. Headers go
// out with the first token, after ttft. It reports when that was and whether the stream finished.
func stream(ctx context.Context, w http.ResponseWriter, anthropic bool, req request, p profile, id string, prompt int) (time.Duration, bool) {
	began := time.Now()
	if !sleep(ctx, p.ttft) {
		return 0, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	start, tok, end := events(anthropic, req, id, prompt, p.chunks)
	// Each token is "@<send time in ns> ", so the client can tell how long each chunk spent in
	// the gateway. The mock and load generator share one clock (one kernel).
	pre, post, _ := strings.Cut(tok, "@")
	buf := make([]byte, 0, len(start)+len(tok)+20)

	var tick <-chan time.Time
	if p.gap > 0 {
		t := time.NewTicker(p.gap)
		defer t.Stop()
		tick = t.C
	}
	var first time.Duration
	for i := 0; i < p.chunks; i++ {
		if i > 0 && tick != nil {
			select {
			case <-tick:
			case <-ctx.Done():
				return 0, false
			}
		}
		buf = buf[:0]
		if i == 0 {
			buf = append(buf, start...)
		}
		buf = append(strconv.AppendInt(append(append(buf, pre...), '@'), time.Now().UnixNano(), 10), post...)
		if _, err := w.Write(buf); err != nil {
			return 0, false
		}
		if rc.Flush() != nil {
			return 0, false
		}
		if i == 0 {
			first = time.Since(began)
		}
	}
	if _, err := io.WriteString(w, end); err != nil {
		return 0, false
	}
	return first, rc.Flush() == nil
}

// events returns the opening events, one content event (sent once per output token) and the
// closing events of a stream.
func events(anthropic bool, req request, id string, prompt, chunks int) (start, tok, end string) {
	if anthropic {
		start = fmt.Sprintf(`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_%s","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n\n", id, req.Model, prompt) +
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			`event: ping` + "\n" + `data: {"type":"ping"}` + "\n\n"
		tok = `event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"@ "}}` + "\n\n"
		end = `event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
			fmt.Sprintf(`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":%d}}`+"\n\n", chunks) +
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n"
		return start, tok, end
	}
	// OpenAI sends "usage":null on every chunk when the client asked for usage, then one
	// chunk with empty choices that carries it.
	usage := ""
	if req.StreamOptions.IncludeUsage {
		usage = `,"usage":null`
	}
	head := fmt.Sprintf(`data: {"id":"chatcmpl-%s","object":"chat.completion.chunk","created":%d,"model":%q,"system_fingerprint":null,"choices":[{"index":0,"delta":`, id, time.Now().Unix(), req.Model)
	start = head + `{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]` + usage + "}\n\n"
	tok = head + `{"content":"@ "},"logprobs":null,"finish_reason":null}]` + usage + "}\n\n"
	end = head + `{},"logprobs":null,"finish_reason":"stop"}]` + usage + "}\n\n"
	if req.StreamOptions.IncludeUsage {
		end += fmt.Sprintf(`data: {"id":"chatcmpl-%s","object":"chat.completion.chunk","created":%d,"model":%q,"system_fingerprint":null,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`+"\n\n",
			id, time.Now().Unix(), req.Model, prompt, chunks, prompt+chunks)
	}
	return start, tok, end + "data: [DONE]\n\n"
}

func whole(anthropic bool, model, id string, prompt, chunks int) string {
	text := strings.Repeat("lorem ", chunks)
	if anthropic {
		return fmt.Sprintf(`{"id":"msg_%s","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
			id, model, text, prompt, chunks)
	}
	return fmt.Sprintf(`{"id":"chatcmpl-%s","object":"chat.completion","created":%d,"model":%q,"system_fingerprint":null,"choices":[{"index":0,"message":{"role":"assistant","content":%q,"refusal":null},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		id, time.Now().Unix(), model, text, prompt, chunks, prompt+chunks)
}

func writeError(w http.ResponseWriter, anthropic bool, status int) {
	kind, msg := "server_error", "mock upstream error"
	if status == http.StatusTooManyRequests {
		kind = "rate_limit_error"
		w.Header().Set("Retry-After", "7")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if anthropic {
		fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, kind, msg)
		return
	}
	fmt.Fprintf(w, `{"error":{"message":%q,"type":%q,"param":null,"code":null}}`, msg, kind)
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// lgid finds the load generator's LGID:<n>: tag in a request body.
func lgid(body []byte) (int64, bool) {
	i := bytes.Index(body, []byte("LGID:"))
	if i < 0 {
		return 0, false
	}
	rest := body[i+5:]
	j := bytes.IndexByte(rest, ':')
	if j < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(rest[:j]), 10, 64)
	return n, err == nil
}

func models(w http.ResponseWriter, _ *http.Request) {
	var data []map[string]any
	for name := range profiles {
		// Fields for both the OpenAI and Anthropic model-list shapes.
		data = append(data, map[string]any{"id": name, "object": "model", "type": "model", "created": 0,
			"created_at": "2026-01-01T00:00:00Z", "owned_by": "mock", "display_name": name})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "has_more": false})
}

func statsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	last, _ := stats.lastNotFound.Load().(string)
	json.NewEncoder(w).Encode(map[string]any{
		"requests": stats.requests.Load(), "in_flight": stats.inFlight.Load(),
		"max_in_flight": stats.maxInFlight.Load(), "completed": stats.completed.Load(),
		"cancelled": stats.cancelled.Load(), "failed": stats.failed.Load(), "conns": stats.conns.Load(),
		"prompt_tokens": stats.promptTokens.Load(), "completion_tokens": stats.completionTokens.Load(),
		"usage_requested": stats.usageRequested.Load(), "not_found": stats.notFound.Load(), "last_not_found": last,
	})
}

// timingsHandler writes every recorded timing as three little-endian int64s.
func timingsHandler(w http.ResponseWriter, _ *http.Request) {
	timingsMu.Lock()
	t := timings
	timingsMu.Unlock()
	w.Header().Set("Content-Type", "application/octet-stream")
	binary.Write(w, binary.LittleEndian, t)
}

func reset(w http.ResponseWriter, _ *http.Request) {
	for _, c := range []*atomic.Int64{&stats.requests, &stats.maxInFlight, &stats.completed, &stats.cancelled,
		&stats.failed, &stats.conns, &stats.promptTokens, &stats.completionTokens, &stats.usageRequested, &stats.notFound} {
		c.Store(0)
	}
	stats.maxInFlight.Store(stats.inFlight.Load())
	timingsMu.Lock()
	timings = nil
	timingsMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
