// Command loadgen drives the gateway benchmarks. It sends OpenAI or Anthropic requests at a
// fixed arrival rate, an open model where a slow gateway can't slow the senders down and hide
// its own stalls, and reports latency from each request's scheduled start, time to first token
// and the gaps between streamed tokens. With -mock it joins the mock upstream's own timing for
// every request, so the gateway's overhead is measured per request rather than by subtracting
// percentiles.
//
//	loadgen -url http://router:8080/v1/chat/completions -H 'Authorization: Bearer llmr_…' -rate 500
//	loadgen report results/2026-10-07
//	loadgen usage results/2026-10-07/samples-s2-router.txt router [FROM_MS TO_MS]
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	target    = flag.String("url", "", "endpoint, e.g. http://router:8080/v1/chat/completions")
	api       = flag.String("api", "openai", "request and stream shape: openai or anthropic")
	model     = flag.String("model", "fast", "model to request")
	rate      = flag.Float64("rate", 100, "requests per second")
	conc      = flag.Int("concurrency", 0, "instead of -rate, keep this many requests in flight back to back (closed loop)")
	duration  = flag.Duration("duration", 30*time.Second, "measured duration")
	warmup    = flag.Duration("warmup", 5*time.Second, "unmeasured lead-in at the same rate")
	stream    = flag.Bool("stream", false, "ask for a streamed response")
	usage     = flag.Bool("include-usage", false, "set stream_options.include_usage (OpenAI)")
	prompt    = flag.Int("prompt", 2048, "user prompt size in bytes")
	system    = flag.Int("system", 0, "system prompt size in bytes")
	tools     = flag.Int("tools", 0, "tool definitions to include")
	turns     = flag.Int("turns", 0, "earlier user/assistant turns to include")
	maxTokens = flag.Int("max-tokens", 1024, "max_tokens")
	timeout   = flag.Duration("timeout", 60*time.Second, "per-request timeout")
	cancelTok = flag.Int("cancel-tokens", 0, "hang up after this many streamed tokens")
	cancelAt  = flag.Duration("cancel-at", 0, "hang up after this long")
	expect    = flag.Int("expect", 0, "tokens every stream must carry; fewer counts as an error")
	mockURL   = flag.String("mock", "", "mock base URL: reset it before, then join its per-request timings")
	out       = flag.String("out", "", "write the JSON summary here instead of stdout")
	print1    = flag.Bool("print", false, "print the first response body to stderr")
	maxFlight = flag.Int64("max-in-flight", 60000, "skip a request rather than exceed this many in flight")
	headers   headerList
	tags      tagList
)

type headerList [][2]string

func (h *headerList) String() string { return fmt.Sprint(*h) }
func (h *headerList) Set(s string) error {
	k, v, ok := strings.Cut(s, ":")
	if !ok {
		return fmt.Errorf("header %q is not 'Name: value'", s)
	}
	*h = append(*h, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
	return nil
}

type tagList map[string]string

func (t *tagList) String() string { return fmt.Sprint(*t) }
func (t *tagList) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("tag %q is not key=value", s)
	}
	if *t == nil {
		*t = tagList{}
	}
	(*t)[k] = v
	return nil
}

type result struct {
	id                int64 // the LGID in the prompt
	scheduled         bool
	latency           time.Duration // from the scheduled start: what a user waits
	lag               time.Duration // scheduled start to actual send: the load generator's own delay
	ttfb, ttft, total time.Duration // from the actual send
	status            int
	err               string
	cancelled         bool
	tokens            int
	gaps              []float32 // ms between streamed tokens
	delays            []float32 // ms from the mock sending each token to its arrival here
}

var client = &http.Client{Transport: &http.Transport{
	DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	// Go keeps 2 idle connections per host by default; anything above that would turn the
	// test into a TCP connect benchmark.
	MaxIdleConnsPerHost: 1 << 20,
	IdleConnTimeout:     2 * time.Minute,
	DisableCompression:  true,
}}

func main() {
	if len(os.Args) > 2 && (os.Args[1] == "report" || os.Args[1] == "usage" || os.Args[1] == "summary") {
		var err error
		switch os.Args[1] {
		case "report":
			err = report(os.Args[2], os.Stdout)
		case "summary": // summary DIR [DOC]: print the tables, or write them into DOC
			if len(os.Args) > 3 {
				err = spliceDoc(os.Args[2], os.Args[3])
			} else {
				err = summarizeDir(os.Args[2], os.Stdout)
			}
		default:
			err = usageCmd(os.Args[2:])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	flag.Var(&headers, "H", "request header 'Name: value' (repeatable)")
	flag.Var(&tags, "tag", "key=value stored in the summary (repeatable)")
	flag.Parse()
	if *target == "" || (*rate <= 0 && *conc <= 0) {
		flag.Usage()
		os.Exit(2)
	}
	if *mockURL != "" {
		if _, err := post(*mockURL + "/reset"); err != nil {
			fmt.Fprintln(os.Stderr, "reset mock:", err)
			os.Exit(1)
		}
	}

	prefix, suffix := buildBody()
	t0 := time.Now().Add(50 * time.Millisecond)
	var results []result
	if *conc > 0 {
		results = closedLoop(prefix, suffix, t0)
	} else {
		results = openLoop(prefix, suffix, t0)
	}

	s := summarize(results, t0)
	s.BodyBytes = len(prefix) + len(suffix) + len(strconv.Itoa(len(results)))
	if *mockURL != "" {
		joinMock(&s, results)
	}
	enc := json.NewEncoder(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		enc = json.NewEncoder(f)
	}
	enc.SetIndent("", "  ")
	enc.Encode(s)
	fmt.Fprintf(os.Stderr, "%s rate=%.0f achieved=%.1f ok=%d/%d errors=%v p50=%.2fms p99=%.2fms overhead p50=%.3fms p99=%.3fms lag p99=%.2fms\n",
		*model, *rate, s.Achieved, s.OK, s.Sent, s.Errors, s.Latency["p50"], s.Latency["p99"], s.Overhead["p50"], s.Overhead["p99"], s.Lag["p99"])
}

// errHangUp marks a request the load generator abandoned on purpose (-cancel-at, -cancel-tokens).
var errHangUp = errors.New("hung up on purpose")

// openLoop starts request i at t0 + i/rate whether or not earlier ones have finished.
func openLoop(prefix, suffix []byte, t0 time.Time) []result {
	n := int(*rate * (*warmup + *duration).Seconds())
	interval := float64(time.Second) / *rate
	results := make([]result, n)
	var wg sync.WaitGroup
	var inFlight atomic.Int64
	for i := range n {
		at := t0.Add(time.Duration(float64(i) * interval))
		if d := time.Until(at); d > 0 {
			time.Sleep(d)
		}
		measured := at.Sub(t0) >= *warmup
		if inFlight.Load() >= *maxFlight {
			results[i] = result{id: int64(i), scheduled: measured, err: "client_overload"}
			continue
		}
		wg.Add(1)
		inFlight.Add(1)
		go func() {
			defer wg.Done()
			defer inFlight.Add(-1)
			r := do(at, body(prefix, suffix, int64(i)), i == 0 && *print1)
			r.id, r.scheduled = int64(i), measured
			results[i] = r
		}()
	}
	wg.Wait()
	return results
}

// closedLoop keeps -concurrency requests in flight, each worker sending its next request as soon
// as the last one finishes, until the window ends. Latency then includes any queueing in the
// gateway, so beyond concurrency 1 it mostly restates throughput (concurrency / rate).
func closedLoop(prefix, suffix []byte, t0 time.Time) []result {
	var next atomic.Int64
	end := t0.Add(*warmup + *duration)
	per := make([][]result, *conc)
	var wg sync.WaitGroup
	for w := range per {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Until(t0))
			for now := time.Now(); now.Before(end); now = time.Now() {
				id := next.Add(1) - 1
				r := do(now, body(prefix, suffix, id), id == 0 && *print1)
				r.id, r.scheduled = id, now.Sub(t0) >= *warmup
				per[w] = append(per[w], r)
			}
		}()
	}
	wg.Wait()
	return slices.Concat(per...)
}

func body(prefix, suffix []byte, id int64) []byte {
	b := make([]byte, 0, len(prefix)+len(suffix)+20)
	return append(strconv.AppendInt(append(b, prefix...), id, 10), suffix...)
}

func do(at time.Time, body []byte, show bool) (r result) {
	parent, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx, stop := context.WithTimeout(parent, *timeout)
	defer stop()
	if *cancelAt > 0 {
		t := time.AfterFunc(*cancelAt, func() { cancel(errHangUp) })
		defer t.Stop()
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, *target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if *api == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}

	start := time.Now()
	r.lag = start.Sub(at)
	defer func() { r.latency = time.Since(at) }()
	resp, err := client.Do(req)
	if err != nil {
		r.err = classify(ctx, err)
		r.cancelled = r.err == "cancelled"
		return r
	}
	defer resp.Body.Close()
	r.ttfb = time.Since(start)
	r.status = resp.StatusCode
	var sink io.Writer = io.Discard
	if show {
		sink = os.Stderr
		defer fmt.Fprintln(os.Stderr)
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(sink, resp.Body)
		r.err = "status_" + strconv.Itoa(resp.StatusCode)
		return r
	}
	if !*stream {
		if _, err := io.Copy(sink, resp.Body); err != nil {
			r.err = classify(ctx, err)
			r.cancelled = r.err == "cancelled"
		}
		r.total = time.Since(start)
		return r
	}

	br := bufio.NewReaderSize(resp.Body, 32<<10)
	var last time.Time
	var done bool
	for {
		line, err := br.ReadSlice('\n')
		if show {
			sink.Write(line)
		}
		if p, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			now := time.Now()
			if bytes.Contains(p, []byte("[DONE]")) || bytes.Contains(p, []byte("message_stop")) {
				done = true
			}
			if isToken(p) {
				r.tokens++
				if r.tokens == 1 {
					r.ttft = now.Sub(start)
				} else {
					r.gaps = append(r.gaps, float32(now.Sub(last).Seconds()*1000))
				}
				last = now
				if ns, ok := stamp(p); ok {
					r.delays = append(r.delays, float32(float64(now.UnixNano()-ns)/1e6))
				}
				if *cancelTok > 0 && r.tokens >= *cancelTok {
					cancel(errHangUp)
					r.cancelled = true
					r.total = time.Since(start)
					return r
				}
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // a long line; its tail has no "data:" prefix and is skipped
		}
		if err != nil {
			if err != io.EOF {
				r.err = classify(ctx, err)
				r.cancelled = r.err == "cancelled"
			}
			break
		}
	}
	r.total = time.Since(start)
	switch {
	case r.err != "":
	case !done:
		r.err = "truncated"
	case *expect > 0 && r.tokens < *expect:
		r.err = "short"
	}
	return r
}

// isToken reports whether an SSE data payload carries a streamed token: an OpenAI delta with a
// non-empty "content", or an Anthropic text_delta. Python gateways write `"content": "x"` with a
// space and the first OpenAI chunk has an empty string, so the value is checked, not the key.
func isToken(p []byte) bool {
	if bytes.Contains(p, []byte(`text_delta`)) {
		return true
	}
	for rest := p; ; {
		i := bytes.Index(rest, []byte(`"content":`))
		if i < 0 {
			return false
		}
		rest = rest[i+len(`"content":`):]
		v := bytes.TrimLeft(rest, " ")
		if len(v) > 1 && v[0] == '"' && v[1] != '"' {
			return true
		}
	}
}

// stamp reads the send time the mock put in a token ("@<unix ns>").
func stamp(p []byte) (int64, bool) {
	i := bytes.IndexByte(p, '@')
	if i < 0 {
		return 0, false
	}
	j := i + 1
	for j < len(p) && p[j] >= '0' && p[j] <= '9' {
		j++
	}
	n, err := strconv.ParseInt(string(p[i+1:j]), 10, 64)
	return n, err == nil && j-i > 16
}

func classify(ctx context.Context, err error) string {
	switch {
	case errors.Is(context.Cause(ctx), errHangUp):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	default:
		return "conn"
	}
}

// buildBody returns the request JSON split around the spot where each request's LGID number
// goes, so every prompt is unique and the mock can match it to its own timing.
func buildBody() (prefix, suffix []byte) {
	anthropic := *api == "anthropic"
	var msgs []map[string]any
	if *system > 0 && !anthropic {
		msgs = append(msgs, map[string]any{"role": "system", "content": filler(*system)})
	}
	for range *turns {
		msgs = append(msgs, map[string]any{"role": "user", "content": filler(300)},
			map[string]any{"role": "assistant", "content": filler(300)})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "LGID:{{ID}}: " + filler(*prompt)})
	req := map[string]any{"model": *model, "max_tokens": *maxTokens, "messages": msgs}
	if *stream {
		req["stream"] = true
		if *usage && !anthropic {
			req["stream_options"] = map[string]bool{"include_usage": true}
		}
	}
	if *system > 0 && anthropic {
		req["system"] = []map[string]string{{"type": "text", "text": filler(*system)}}
	}
	if *tools > 0 {
		var defs []map[string]any
		for i := range *tools {
			schema := map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{
				"path":    map[string]string{"type": "string", "description": filler(200)},
				"content": map[string]string{"type": "string", "description": filler(300)},
				"limit":   map[string]string{"type": "integer", "description": filler(100)},
			}}
			name, desc := fmt.Sprintf("tool_%d", i), filler(600)
			if anthropic {
				defs = append(defs, map[string]any{"name": name, "description": desc, "input_schema": schema})
			} else {
				defs = append(defs, map[string]any{"type": "function", "function": map[string]any{"name": name, "description": desc, "parameters": schema}})
			}
		}
		req["tools"] = defs
	}
	b, _ := json.Marshal(req)
	i := bytes.Index(b, []byte("{{ID}}"))
	return b[:i], b[i+len("{{ID}}"):]
}

func filler(n int) string {
	const words = "lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor. "
	return strings.Repeat(words, n/len(words)+1)[:n]
}

type summary struct {
	Tags          map[string]string  `json:"tags"`
	URL           string             `json:"url"`
	API           string             `json:"api"`
	Model         string             `json:"model"`
	Stream        bool               `json:"stream"`
	Rate          float64            `json:"rate"`
	Concurrency   int                `json:"concurrency,omitempty"`
	WindowStartMS int64              `json:"window_start_ms"`
	WindowEndMS   int64              `json:"window_end_ms"`
	WarmupMS      int64              `json:"warmup_ms"`
	BodyBytes     int                `json:"body_bytes"`
	Sent          int                `json:"sent"`
	OK            int                `json:"ok"`
	OKAll         int                `json:"ok_all"` // warmup included, to compare with usage rows and the mock
	Cancelled     int                `json:"cancelled"`
	Errors        map[string]int     `json:"errors"`
	Achieved      float64            `json:"achieved_rps"`
	Latency       map[string]float64 `json:"latency_ms"`
	Service       map[string]float64 `json:"service_ms"`
	TTFB          map[string]float64 `json:"ttfb_ms"`
	TTFT          map[string]float64 `json:"ttft_ms,omitempty"`
	Gap           map[string]float64 `json:"gap_ms,omitempty"`
	ChunkDelay    map[string]float64 `json:"chunk_delay_ms,omitempty"`
	Batched       float64            `json:"batched,omitempty"` // share of token gaps under 1 ms: chunks held back and sent together
	Lag           map[string]float64 `json:"lag_ms"`
	Tokens        map[string]float64 `json:"tokens,omitempty"`
	Overhead      map[string]float64 `json:"overhead_ms,omitempty"`
	OverheadTTFT  map[string]float64 `json:"overhead_ttft_ms,omitempty"`
	Joined        int                `json:"joined,omitempty"`
	Mock          map[string]any     `json:"mock,omitempty"`
}

func summarize(results []result, t0 time.Time) summary {
	s := summary{Tags: tags, URL: *target, API: *api, Model: *model, Stream: *stream, Rate: *rate, Concurrency: *conc,
		WindowStartMS: t0.Add(*warmup).UnixMilli(), WindowEndMS: t0.Add(*warmup + *duration).UnixMilli(),
		WarmupMS: warmup.Milliseconds(), Errors: map[string]int{}}
	var lat, svc, ttfb, ttft, gaps, delays, lag, toks []float64
	for _, r := range results {
		if r.err == "" && !r.cancelled {
			s.OKAll++
		}
		if !r.scheduled {
			continue
		}
		s.Sent++
		lag = append(lag, ms(r.lag))
		switch {
		case r.cancelled:
			s.Cancelled++
		case r.err != "":
			s.Errors[r.err]++
		default:
			s.OK++
			lat = append(lat, ms(r.latency))
			svc = append(svc, ms(r.total))
			ttfb = append(ttfb, ms(r.ttfb))
			if *stream {
				ttft = append(ttft, ms(r.ttft))
				toks = append(toks, float64(r.tokens))
				for _, g := range r.gaps {
					gaps = append(gaps, float64(g))
				}
				for _, d := range r.delays {
					delays = append(delays, float64(d))
				}
			}
		}
	}
	s.Achieved = float64(s.Sent) / duration.Seconds()
	s.Latency, s.Service, s.TTFB, s.Lag = pct(lat), pct(svc), pct(ttfb), pct(lag)
	if *stream {
		s.TTFT, s.Gap, s.Tokens, s.ChunkDelay = pct(ttft), pct(gaps), pct(toks), pct(delays)
		var held int
		for _, g := range gaps {
			if g < 1 {
				held++
			}
		}
		if len(gaps) > 0 {
			s.Batched = float64(held) / float64(len(gaps))
		}
	}
	return s
}

// joinMock subtracts the mock's own time on each request from what the load generator saw, which
// leaves exactly what the gateway (and the extra network hop) added to that request.
func joinMock(s *summary, results []result) {
	if b, err := get(*mockURL + "/stats"); err == nil {
		json.Unmarshal(b, &s.Mock)
	}
	b, err := get(*mockURL + "/timings")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mock timings:", err)
		return
	}
	type timing struct{ ID, First, Last int64 }
	t := make([]timing, len(b)/24)
	binary.Read(bytes.NewReader(b), binary.LittleEndian, t)
	byID := make(map[int64]timing, len(t))
	for _, x := range t {
		byID[x.ID] = x
	}
	var total, first []float64
	for _, r := range results {
		x, ok := byID[r.id]
		if !ok || !r.scheduled || r.err != "" || r.cancelled {
			continue
		}
		s.Joined++
		total = append(total, ms(r.total)-float64(x.Last)/1e6)
		if *stream {
			first = append(first, ms(r.ttft)-float64(x.First)/1e6)
		}
	}
	s.Overhead = pct(total)
	if *stream {
		s.OverheadTTFT = pct(first)
	}
}

func pct(v []float64) map[string]float64 {
	if len(v) == 0 {
		return nil
	}
	slices.Sort(v)
	q := func(p float64) float64 { return v[max(0, int(math.Ceil(p*float64(len(v))))-1)] }
	var sum float64
	for _, x := range v {
		sum += x
	}
	return map[string]float64{"n": float64(len(v)), "mean": sum / float64(len(v)), "min": v[0],
		"p50": q(.5), "p90": q(.9), "p95": q(.95), "p99": q(.99), "p999": q(.999), "max": v[len(v)-1]}
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

func get(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func post(url string) ([]byte, error) {
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
